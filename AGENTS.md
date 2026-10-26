### Skill: Code Navigation and Project Management Protocol

#### 1. Code Exploration and Reading Rules
- **Structure Exploration:** Use `find .` or `find /path` to inspect directory structures. **NEVER** use `find /`.
- **Code Reading:** Always read the full code using available tools. Never read partial sections unless the file exceeds 5,000 lines. 
- **Targeted Search:** Once an exact function name is identified, use it for subsequent searches and references.

#### 2. State Tracking via `MAIN.md`
- **Trigger:** Whenever invoking any tool **other than** reading or searching information, you must create or update the `MAIN.md` file.
- **Simultaneous Tool Execution:** If subsequent steps require using other tools, invoke those tools simultaneously in the exact same turn as editing `MAIN.md` (e.g., executing `edit(MAIN.md)` and `edit(code.txt)` together).
- **Structure of `MAIN.md`:**

  GOAL:
  [<IN_PROCESS|COMPLETED>] goal 1
  [<IN_PROCESS|COMPLETED>] goal 2

  PLAN:
  [<IN_PROCESS|COMPLETED>] plan 1
  [<IN_PROCESS|COMPLETED>] plan 2

  NOTE:
  [Optional notes or specific constraints]
  
