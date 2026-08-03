---
description: Read or set the per-profile DeepSeek reasoning effort mid-session
---

Change or report the effort override for the current DeepSeek profile.

The current profile directory is the value of `CLAUDE_CONFIG_DIR`. Use the
terminal to inspect or update the file `$CLAUDE_CONFIG_DIR/effort-override`.
This is per profile and applies on the next request without restarting the
proxy.

Valid levels are exactly: `max`, `xhigh`, `high`, `medium`, `low`, `minimal`,
`none`.

If the command has no argument (`$ARGUMENTS` is empty), read the file if it
exists and report `current effort: <level>`. If it does not exist, report that
there is no override and effort follows the per-tier defaults from `/model`.
Do not create a file for a query.

If `$ARGUMENTS` contains one valid level, write exactly that level followed by
a newline to `$CLAUDE_CONFIG_DIR/effort-override` (use a temporary file in the
same directory and atomically rename it into place, with no other content),
then report that it applies to the next request.

If the argument is anything else, reject it against the valid set and do not
write anything. Do not accept extra arguments or whitespace-separated values.

The direct `claude-ds4` profile is deliberately unsupported: DeepSeek's own
endpoint ignores `reasoning_effort`, and `output_config.effort` measured
non-monotonic there. If `CLAUDE_CONFIG_DIR` ends in `claude-ds4`, explain that
effort is unproven on the direct path and do not write an override. The
OpenRouter (`claude-or-ds4`) and Nous (`claude-nous`) profiles support it.

Always verify the resulting file contents after a write and keep the response
concise.