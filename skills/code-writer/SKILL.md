---
name: code-writer
description: Generate a new boilerplate file (test scaffolding, fixtures, type definitions, data classes, repetitive code following an existing example) with a worker model (local Ollama or a configured API) instead of writing it token by token. Use only for structured, easily verified code; never for business logic, concurrency or security-sensitive code.
allowed-tools:
  - Bash(code-write *)
  - mcp__plugin_local-shunt_local-shunt__shunt_write
---

# code-writer

A worker model writes one file from your specification. With `--target` the command prints only the path and line count, so the generated code does not enter your context unless you read it.

## Tool or command

If the `shunt_write` MCP tool is available, call it with `out`, `spec`, `context` and optionally `force`. Otherwise run:

```bash
# Generate and write directly to the target file
code-write --spec "<what to generate>" --reference <reference-file>... --target <output-path>

# Print to standard output instead (omit --target)
code-write --spec "<what to generate>" --reference <reference-file>
```

- `--reference`: always pass at least one existing file whose patterns the output should follow, such as an existing test to imitate and the module under test. Without one, the output matches nothing in the project.
- Each call is independent. To build on what was just generated, pass that file as the `--reference` for the next call.
- `--force`: overwrite an existing target. Without it the command refuses.
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
- Naming and import conventions, or a reference file that shows them

## After writing

1. Run the tests, type checker or linter on the file.
2. If they fail, fix the file yourself. Do not delegate the same file again.
3. Read the parts you are unsure about before reporting the work as done.
