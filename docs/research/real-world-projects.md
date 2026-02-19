# HiveKernel vs Real-World Projects

## Use Case: Website Builder (Business Card Site)

### Current Model (One-Shot)
```
User: "research headphones"
Assistant -> Queen -> Orchestrator -> 3 Workers (parallel) -> synthesis -> response
```
All executed in a single pass. Agents die after completion. No context between requests.

### Real Project Model (Multi-Phase, Iterative)
```
User: "build a business card website"
  -> requirements gathering (dialog with user)
  -> design (sequential, needs decisions)
  -> implementation (parallel: HTML, CSS, JS)
  -> review (check result)
  -> revisions from feedback (cycle)
  -> deploy
```

### Proposed Node Structure
```
King (PID 1)
+-- Maid (daemon)
+-- Queen (daemon)
+-- Assistant (agent) -- user chat
|
+-- [Queen spawns per project:]
    |
    Project-PM (architect role, long-lived)
    |   Lives for the entire project duration.
    |   Holds: requirements, decisions, current state.
    |   Plans phases, reviews outputs between phases.
    |
    +-- Phase: Requirements
    |   +-- Interviewer (task) -- generates clarifying questions
    |
    +-- Phase: Design
    |   +-- Orchestrator (lead)
    |       +-- Layout-worker (task) -- page structure
    |       +-- Style-worker (task) -- colors, typography
    |
    +-- Phase: Implementation
    |   +-- Orchestrator (lead)
    |       +-- HTML-worker (task)
    |       +-- CSS-worker (task)
    |       +-- JS-worker (task)
    |
    +-- Phase: Review
    |   +-- QA-worker (task) -- checks code, accessibility
    |
    +-- Phase: Revision (from user feedback)
        +-- Targeted-worker (task) -- fixes specific file
```

### Three Levels, Three Rhythms

| Level | Role | Lifetime | Purpose |
|-------|------|----------|---------|
| PM | architect | entire project (hours/days) | plans phases, holds context, reviews between phases |
| Orchestrator | lead | one phase | decomposes phase into parallel tasks |
| Worker | task | one task | writes specific file/component |

### Gaps Identified

1. **No "project" concept** - each task_request is isolated
2. **No sequential phases** - Orchestrator only does parallel decomposition
3. **No mid-process user feedback** - task sent -> result received, no pause
4. **No long-lived project agents** - task role auto-dies, worker lives per-lead
5. **No shared workspace** - workers output text, need to write actual files

### Root Cause: Agent Memory

All gaps stem from one fundamental problem: **agents have no memory**.
- Workers are stateless one-shot executors
- No conversation history persists between tasks
- No way to accumulate project context across phases
- Each LLM call is independent (no tool-calling loop)

See: `docs/research/agent-memory-design.md` for the memory system design.

**RESOLVED by Plan 013** (`docs/plans/013-agent-core.md`): ToolAgent base class
provides tool calling, iterative agent loop, and artifact-backed persistent memory.
Agents can now call tools, make decisions across iterations, and accumulate context.

---

## Future Use Cases (To Analyze)

- [ ] Codebase refactoring (multi-file, multi-phase)
- [ ] Data pipeline builder (ETL with validation)
- [ ] Content marketing (research -> outline -> write -> edit -> publish)
- [ ] Customer support bot (needs conversation memory + knowledge base)
