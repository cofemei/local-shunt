---
name: code-writer
description: Generate a simple, easily verified boilerplate file with local-shunt from existing project examples. Use only for repetitive code, fixtures, schemas, type definitions, or test scaffolding.
---

# code-writer

Call the `shunt_write` MCP tool with the target path, specification, reference-file context, and `force` only when the user authorized an overwrite.

- Always provide one or more existing reference files that establish project conventions.
- State required signatures, cases, names, and imports in the specification; do not leave these to inference.
- Do not use this skill for business logic, concurrency, authentication, authorization, cryptography, or input validation.
- After generation, run the relevant test, type checker, or linter and inspect uncertain code before reporting completion.
