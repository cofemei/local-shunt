---
name: bulk-reader
description: Answer a focused question about one or more large local files with local-shunt instead of reading their full contents. Use for source files roughly 350 lines or longer when an exact edit or security review is not needed.
---

# bulk-reader

Call the `shunt_read` MCP tool with absolute file paths and a specific question. The worker returns a compact answer with line references.

- Ask one focused question and include every relevant file in one call.
- Treat the answer as unverified. Check its uncertainty section and spot-check important claims at the cited lines.
- Read exact source with a limited range when preparing an edit, debugging precise behavior, reviewing security-sensitive code, or checking a literal value.
- If the MCP tool fails, do not retry the same request more than once. Use search and a limited read instead.
