"""MaidAgent -- per-VPS health and cleanup daemon for HiveKernel.

Spawned by Queen on startup. Runs periodic health checks:
- Zombie scan: find zombie processes whose parents didn't reap
- Orphan detection: find processes whose parent is dead/missing
- Process count monitoring
- Anomaly reporting to Queen via escalate syscall

No LLM needed -- pure diagnostics.

Runtime image: hivekernel_sdk.maid:MaidAgent
"""

import asyncio
import json
import logging
import time

from .agent import HiveAgent
from .syscall import SyscallContext
from .types import TaskResult

logger = logging.getLogger("hivekernel.maid")

# Proto enum values for AgentState.
_STATE_ZOMBIE = 5
_STATE_DEAD = 4


class MaidAgent(HiveAgent):
    """Per-VPS health daemon. Scans process table, reports anomalies."""

    def __init__(self):
        super().__init__()
        self._check_interval = 60  # seconds between health checks
        self._max_pid_scan = 200  # scan PIDs 1..N (like /proc)
        self._loop_task = None

    async def on_init(self, config):
        logger.info("Maid daemon starting (PID %d)", self.pid)
        self._loop_task = asyncio.create_task(self._health_loop())

    async def on_shutdown(self, reason):
        if self._loop_task and not self._loop_task.done():
            self._loop_task.cancel()
            try:
                await self._loop_task
            except asyncio.CancelledError:
                pass
        logger.info("Maid daemon shutting down")
        return None

    async def handle_task(self, task, ctx: SyscallContext) -> TaskResult:
        """On-demand health check (Queen can call execute_on)."""
        report = await self._scan()
        summary = self._format_report(report)
        return TaskResult(exit_code=0, output=summary)

    # --- Internal ---

    async def _health_loop(self):
        """Background loop: periodic health checks."""
        await asyncio.sleep(10)  # let system stabilize after boot
        while True:
            try:
                report = await self._scan()
                summary = self._format_report(report)
                logger.info("Health check: %d processes, %d zombies",
                            report["total"], report["zombie_count"])

                # Store latest health report as artifact.
                if self.core:
                    try:
                        await self.core.store_artifact(
                            key="maid/health-report",
                            content=json.dumps(report, ensure_ascii=False).encode(),
                            content_type="application/json",
                        )
                    except Exception as e:
                        logger.warning("Failed to store health artifact: %s", e)

                # Escalate if anomalies found.
                if report["anomalies"]:
                    anomaly_msg = "; ".join(report["anomalies"])
                    logger.warning("Anomalies detected: %s", anomaly_msg)
                    if self.core:
                        try:
                            await self.core.escalate(
                                issue=f"Maid health alert: {anomaly_msg}",
                                severity="warning",
                            )
                        except Exception as e:
                            logger.warning("Failed to escalate: %s", e)

            except asyncio.CancelledError:
                raise
            except Exception as e:
                logger.error("Health check error: %s", e)

            await asyncio.sleep(self._check_interval)

    async def _scan(self) -> dict:
        """Scan process table by iterating PIDs (like reading /proc)."""
        processes = []
        zombies = []
        anomalies = []

        if not self.core:
            return {
                "ts": time.time(),
                "total": 0,
                "zombie_count": 0,
                "orphan_count": 0,
                "zombies": [],
                "orphans": [],
                "anomalies": ["no core client"],
                "processes": [],
            }

        for pid in range(1, self._max_pid_scan + 1):
            try:
                info = await self.core.get_process_info(pid=pid)
            except Exception:
                continue  # PID doesn't exist

            proc = {
                "pid": info.pid,
                "ppid": info.ppid,
                "name": info.name,
                "state": info.state,
                "role": info.role,
                "tokens": info.tokens_consumed,
            }
            processes.append(proc)

            if info.state == _STATE_ZOMBIE:
                zombies.append(proc)

        # Detect anomalies.
        if zombies:
            anomalies.append(f"{len(zombies)} zombie(s): "
                             + ", ".join(f"PID {z['pid']} ({z['name']})" for z in zombies))

        # Detect orphans: processes whose parent is dead/zombie/missing.
        pid_states = {p["pid"]: p["state"] for p in processes}
        orphans = []
        for proc in processes:
            ppid = proc["ppid"]
            if ppid == 0:
                continue  # King has no parent
            if proc["state"] in (_STATE_ZOMBIE, _STATE_DEAD):
                continue  # Dead/zombie processes aren't orphans, they're done
            if ppid not in pid_states:
                orphans.append(proc)  # Parent not found at all
            elif pid_states[ppid] in (_STATE_ZOMBIE, _STATE_DEAD):
                orphans.append(proc)  # Parent is zombie or dead

        if orphans:
            anomalies.append(f"{len(orphans)} orphan(s): "
                             + ", ".join(f"PID {o['pid']} ({o['name']})" for o in orphans))

        return {
            "ts": time.time(),
            "total": len(processes),
            "zombie_count": len(zombies),
            "orphan_count": len(orphans),
            "zombies": zombies,
            "orphans": orphans,
            "anomalies": anomalies,
            "processes": processes,
        }

    def _format_report(self, report: dict) -> str:
        """Human-readable health report."""
        lines = [
            f"=== Maid Health Report ===",
            f"Time: {time.strftime('%H:%M:%S')}",
            f"Total processes: {report['total']}",
            f"Zombies: {report['zombie_count']}",
            f"Orphans: {report.get('orphan_count', 0)}",
        ]
        if report["zombies"]:
            for z in report["zombies"]:
                lines.append(f"  ZOMBIE: PID {z['pid']} ({z['name']})")
        if report.get("orphans"):
            for o in report["orphans"]:
                lines.append(f"  ORPHAN: PID {o['pid']} ({o['name']}, ppid={o['ppid']})")
        if report["anomalies"]:
            lines.append("Anomalies:")
            for a in report["anomalies"]:
                lines.append(f"  - {a}")
        else:
            lines.append("No anomalies detected.")
        return "\n".join(lines)
