# Apply Patch guidance

Use `apply_patch` for workspace file changes. Prefer the smallest exact edit that leaves the file complete and valid.

- Read the relevant file before editing and preserve its formatting and line endings.
- Use `old_text` only when it identifies exactly one location. Use `expected_sha256` when concurrent modification is plausible.
- Use `operations` for an atomic multi-file transaction, `move_to` for a rename, and `unified_diff` for hunk-oriented changes.
- An `operations` array may contain the same path more than once. Operations run in array order against the cumulative in-memory result, then all final file states are committed atomically; use this when several exact replacements target one file.
- For a new file, provide its complete literal contents. Never use omission placeholders.
- Do not edit symbolic links or paths outside configured write roots.
- After a material edit, run focused verification and inspect the resulting diff when useful.
