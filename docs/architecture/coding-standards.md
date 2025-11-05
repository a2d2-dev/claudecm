# Coding Standards

## Core Standards

- **Language & Runtime:** Go 1.21+
- **Style & Linting:**
  - `gofmt` for code formatting (enforced in CI)
  - `golangci-lint` with default rules + `gocyclo`, `goconst`, `misspell`
- **Test Organization:**
  - Test files alongside source: `manager_test.go` next to `manager.go`
  - Table-driven tests for multiple inputs
  - Integration tests in `_test` packages

## Naming Conventions

| Element | Convention | Example |
|---------|------------|---------|
| Packages | lowercase, no underscores | `config`, `storage` |
| Types | PascalCase | `Profile`, `ConfigManager` |
| Interfaces | PascalCase, often ends in -er | `Validator`, `Storage` |
| Functions | camelCase (public), camelCase (private) | `AddProfile()`, `validateURL()` |
| Constants | PascalCase or SCREAMING_SNAKE | `DefaultTimeout`, `MAX_RETRIES` |
| Files | snake_case | `config_manager.go` |

## Critical Rules

1. **Error Handling:** Every function that can fail MUST return `error` - never panic in library code, only in `main()`

2. **File Permissions:** All config files MUST be created with `0600` (owner read/write only)

3. **Config Validation:** ALWAYS validate profiles before saving - use `validator.ValidateProfile()`, never skip

4. **State Consistency:** State file MUST be updated atomically - write to temp file, then rename

5. **No Global State:** Avoid package-level variables - pass dependencies explicitly (except logger)

6. **Context Propagation:** Long-running operations MUST accept `context.Context`

## Example - Atomic File Write

```go
// DON'T:
func SaveProfile(profile Profile) error {
    return os.WriteFile(path, data, 0600)  // ❌ Not atomic
}

// DO:
func SaveProfile(profile Profile) error {
    tmpFile := path + ".tmp"
    if err := os.WriteFile(tmpFile, data, 0600); err != nil {
        return err
    }
    return os.Rename(tmpFile, path)  // ✅ Atomic
}
```

## Error Handling Pattern

```go
// Wrap errors with context
if err := storage.SaveProfile(profile); err != nil {
    return fmt.Errorf("failed to save profile %s: %w", profile.Name, err)
}
```

## Logging Standards

- Use `log/slog` for structured logging
- Never log sensitive data (auth tokens)
- Include operation context in error messages
