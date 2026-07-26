# TUI Task Configuration

Use `n` from the task board to load a new Standard authoring task from a JSON
file. The TUI loads the file into the existing form, where every value can be
reviewed and corrected before pressing Enter to create the task.

The file must be UTF-8 JSON with exactly one document, no duplicate or unknown
fields, `format: "harbor.task-input.v1"`, and `version: "1"`.

All fields in the example are required except that `is_0_to_1` is a boolean.
`commit_sha` must be a lowercase 40- or 64-character hexadecimal commit ID.
The remaining string fields use the same length limits as their TUI inputs;
the loader trims surrounding whitespace before showing the editable form.

```json
{
  "format": "harbor.task-input.v1",
  "version": "1",
  "repository_url": "https://github.com/rust-lang/rustlings.git",
  "commit_sha": "734461f2fb8c7bb8403f4a9bd1fc7f983d32860b",
  "base_image": "docker.io/library/rust:1.85.1-bookworm@sha256:e51d0265072d2d9d5d320f6a44dde6b9ef13653b035098febd68cce8fa7c0bc4",
  "slug": "rustlings-empty-input-bugfix",
  "title": "Fix empty input handling",
  "task_type": "bugfix",
  "application": "rustlings",
  "code_language": "rust",
  "is_0_to_1": false,
  "objective": "Handle empty input without a panic and add regression coverage.",
  "reason": "Create a bounded authoring task from the reviewed configuration."
}
```

The loader accepts ordinary non-symlink files up to 1 MiB. It rejects malformed
JSON, duplicate keys, unknown fields, a second JSON document, wrong
format/version values, and invalid basic field values. Invalid files do not
overwrite the current form values and can be corrected then loaded again.

Loading does not create an idempotency key, contact a repository, or start a
task. Those actions happen only after the loaded form is reviewed and submitted.
