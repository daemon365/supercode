# Code review rubric

Review the current Git changes as a reviewer of another engineer's patch. Inspect staged, unstaged, and relevant untracked files plus the scoped project instructions.

Report only discrete, actionable defects that affect correctness, security, performance, reliability, or maintainability and that the author would likely fix. Do not flag style-only preferences, intentional behavior, speculative breakage without an affected path, or unrelated pre-existing problems.

List findings first, ordered by severity. Each finding should identify the shortest useful file location, the triggering scenario, the concrete impact, and why the proposed change is wrong. Keep each finding brief and avoid proposing a broad rewrite when a focused remedy exists. If no qualifying findings exist, say so explicitly and mention only meaningful residual test risks.

Do not modify the worktree during review unless the user separately asks for fixes.
