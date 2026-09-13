---
name: code-writer
description: Generate a new boilerplate file (test scaffolding, fixtures, type definitions, data classes, repetitive code following an existing example) with a worker model (local Ollama or a configured API) instead of writing it token by token. Use only for structured, easily verified code; never for business logic, concurrency or security-sensitive code.
allowed-tools:
  - Bash(python3 "${CLAUDE_PLUGIN_ROOT}/scripts/shunt.py" write *)
  - mcp__plugin_local-shunt_local-shunt__shunt_write
---

# code-writer

A worker model writes one file from your specification. The command prints only the path and line count, so the generated code does not enter your context unless you read it.

## Tool or command

If the `shunt_write` MCP tool is available, call it with `out`, `spec` and optionally `context` and `force`. Otherwise run:

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/shunt.py" write \
  --out <path> \
  --spec "<specification>" \
  --context <file>...
```

- `--context`: files the model should follow, such as the module under test or an existing test file to imitate.
- `--force`: overwrite an existing file. Without it the command refuses.
- `--max-output <tokens>`: default 4096. The command writes nothing if the output is cut off.

## Suitable

- Unit test skeletons and fixtures
- Type definitions, schemas, data classes
- Repetitive code that follows an existing example in the repository

## Not suitable

- Business logic
- Concurrency, locking, retries
- Authentication, authorization, cryptography, input validation
- Anything that is hard to verify by running tests or a linter

## Write a good specification

State everything the model must not guess:

- File purpose and language
- Function or class signatures
- The list of test cases, each with input and expected result
- Naming and import conventions, or a context file that shows them

## After writing

1. Run the tests, type checker or linter on the file.
2. If they fail, fix the file yourself. Do not delegate the same file again.
3. Read the parts you are unsure about before reporting the work as done.
