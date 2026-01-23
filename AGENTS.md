# Agent Instructions

## Development Environment

Use Docker to develop and test the application. Do not rely on locally installed Go toolchains.

- **Go version**: 1.24 (latest stable)
- **Dependency management**: Go modules (`go mod`)

## Build Commands

```bash
# Build the image
docker build -t mbproxy .

# Run the proxy
docker run --rm -e MODBUS_UPSTREAM=192.168.1.100:502 mbproxy

# Run with debug logging
docker run --rm -e MODBUS_UPSTREAM=192.168.1.100:502 -e LOG_LEVEL=DEBUG mbproxy
```

## Testing

```bash
# Run all tests with race detector (uses full golang image)
docker build --target test .

# Or run tests interactively (without race detector in alpine)
docker run --rm -v $(pwd):/app -w /app golang:1.24 go test -v ./...

# With race detector (requires full golang image, not alpine)
docker run --rm -v $(pwd):/app -w /app golang:1.24 go test -v -race ./...
```

Note: Race detector requires CGO, which is not available in alpine images. The Dockerfile test stage uses the full golang image for this reason.

## CI Checks

Run the full CI suite locally before committing:

```bash
# Run all CI checks (format, vet, test with race detector)
docker run --rm -v $(pwd):/app -w /app golang:1.24 sh -c "go fmt ./... && go vet ./... && go test -v -race ./..."
```

The CI pipeline runs:
1. **Format check**: `go fmt ./...` followed by `git diff --exit-code` (fails if formatting changes files)
2. **Vet**: `go vet ./...`
3. **Test**: `go test -v -race ./...`

## Code Style

- Run `go fmt` on all code before committing
- Run `go vet` to catch common issues
- Keep functions small and focused
- Use meaningful variable names
- Add comments for non-obvious logic

## Commit Conventions

- Use present tense: "add feature" not "added feature"
- Keep subject line under 50 characters
- Format: `<type>: <description>`
- Types: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`
- Always commit topic by topic (one logical change per commit)
- Never push automatically; always wait for explicit user approval

Examples:
```
feat: add request coalescing for cache misses
fix: handle upstream connection timeout
refactor: extract cache logic to separate package
```

## Debugging

Set `LOG_LEVEL=DEBUG` to enable verbose logging:

```bash
LOG_LEVEL=DEBUG docker run --rm -e MODBUS_UPSTREAM=... mbproxy
```

Debug logs include:
- Cache hits/misses
- Upstream request timing
- Connection events
- Request coalescing activity

## Go Best Practices

### Error Handling
- Always handle errors; never discard with `_`
- Return errors instead of panicking for normal error handling
- Error strings should be lowercase, no punctuation: `fmt.Errorf("connection failed")` not `"Connection failed."`
- Indent error handling, keep happy path at minimal indentation:
  ```go
  // Good
  if err != nil {
      return err
  }
  // normal code

  // Bad
  if err != nil {
      // error handling
  } else {
      // normal code
  }
  ```

### Naming
- Variable names: short for local scope (`c` not `lineCount`), descriptive for wider scope
- Receiver names: 1-2 letter abbreviation (`c` for `Client`), never `this`/`self`/`me`
- Initialisms: `ID`, `URL`, `HTTP` stay uppercase (`userID` not `userId`)

### Goroutines
- Document when and whether goroutines exit
- Ensure goroutines don't leak by blocking on channels
- Prefer synchronous functions; let callers add concurrency if needed

### Imports
- Group standard library first, blank line, then external packages
- Use `goimports` to auto-format

### Slices and Interfaces
- Prefer `var t []string` over `t := []string{}` for empty slices
- Define interfaces where used, not where implemented
