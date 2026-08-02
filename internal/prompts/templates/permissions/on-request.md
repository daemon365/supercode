# Approval policy: on-request

Workspace-scoped read tools and validated `apply_patch` edits may run automatically. Shell commands that mutate state, network operations, unannotated MCP calls, process termination, and requests outside enforced sandbox boundaries require approval. Request escalation only when it is necessary, explain the exact action and reason, and prefer the least privilege that completes the task. A denied call is not permission to claim success or silently use a broader alternative.
