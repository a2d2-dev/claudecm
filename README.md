# claudecm - Claude Code Environment Manager

`claudecm` is a CLI tool for managing Claude Code environment configurations. Easily switch between multiple API configurations for different environments, providers, or projects.

## Features

- 🚀 **Fast switching** - Change environments in milliseconds
- 📝 **Human-readable** - YAML configuration format
- 🎨 **Interactive UI** - Beautiful terminal prompts and selection
- 💻 **Cross-platform** - Works on macOS, Linux, and Windows

> **Storage:** Profiles stored as plaintext YAML in `~/.claudecm/` with file mode `0600`. Encryption is deferred post-v1.

## Installation

### From Source

```bash
git clone https://github.com/a2d2-dev/claudecm
cd claudecm
make install
```

### Using Go

```bash
go install github.com/a2d2-dev/claudecm@latest
```

## Quick Start

### 1. Add your first profile

```bash
claudecm add
```

The `add` command will:
1. Extract current Claude environment variables (`ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_MODEL`, `ANTHROPIC_SMALL_FAST_MODEL`)
2. Display them for review
3. Allow you to edit the values
4. Prompt for a profile name
5. Save the profile and show the profile list

### 2. List all profiles

```bash
claudecm list
```

### 3. Switch between profiles

```bash
claudecm switch
```

Or switch directly:

```bash
claudecm switch anthropic-us
```

### 4. Export environment variables

```bash
eval $(claudecm export)
```

This will set the following environment variables:
- `ANTHROPIC_BASE_URL`
- `ANTHROPIC_AUTH_TOKEN`
- `ANTHROPIC_MODEL` (if configured)
- `ANTHROPIC_SMALL_FAST_MODEL` (if configured)
- Any custom environment variables

### 5. Delete a profile

```bash
claudecm delete
```

## Commands

| Command | Description |
|---------|-------------|
| `claudecm add` | Add a new profile from current environment |
| `claudecm list` | List all profiles |
| `claudecm switch [name]` | Switch active profile (with tab completion) |
| `claudecm export` | Export environment variables |
| `claudecm delete [name]` | Delete a profile (with tab completion) |
| `claudecm completion [bash\|zsh\|fish\|powershell]` | Generate shell completion script |
| `claudecm version` | Show version information |
| `claudecm help` | Show help |

## Shell Completion

`claudecm` supports shell completion for all major shells. This enables tab completion for commands and profile names.

### Quick Setup (Recommended)

```bash
claudecm completion install
```

This will automatically detect your shell and install the appropriate completion script.

### Manual Setup

### Zsh (Recommended for macOS)

Add to your `~/.zshrc`:

```bash
# Enable completion
autoload -U compinit; compinit

# Load claudecm completion
source <(claudecm completion zsh)
```

Or install permanently:

```bash
claudecm completion zsh > "${fpath[1]}/_claudecm"
```

Then restart your shell or run `source ~/.zshrc`.

### Bash

```bash
# Load for current session
source <(claudecm completion bash)

# Load permanently (Linux)
claudecm completion bash > /etc/bash_completion.d/claudecm

# Load permanently (macOS with Homebrew)
claudecm completion bash > $(brew --prefix)/etc/bash_completion.d/claudecm
```

### Fish

```bash
# Load for current session
claudecm completion fish | source

# Load permanently
claudecm completion fish > ~/.config/fish/completions/claudecm.fish
```

### PowerShell

```powershell
# Load for current session
claudecm completion powershell | Out-String | Invoke-Expression

# Add to your PowerShell profile for permanent loading
```

After setting up completion, you can:
- Press `Tab` after `claudecm switch` to see available profiles
- Press `Tab` after `claudecm delete` to see available profiles
- Press `Tab` after `claudecm` to see all available commands

## Configuration

Configurations are stored in `~/.claudecm/`:

```
~/.claudecm/
├── profiles/
│   ├── anthropic-us.yaml
│   ├── anthropic-cn.yaml
│   └── moonshot-dev.yaml
└── state.yaml
```

### Profile File Format

```yaml
name: anthropic-us
base_url: https://api.anthropic.com
auth_token: sk-ant-api03-xxxxx
model: claude-sonnet-4
description: "Anthropic US Production API"
custom_env:
  ANTHROPIC_TIMEOUT: "60"
  ANTHROPIC_MAX_RETRIES: "3"
created_at: 2025-10-31T10:30:00Z
updated_at: 2025-10-31T10:30:00Z
```

## Development

### Prerequisites

- Go 1.21+
- Make

### Build

```bash
make build
```

### Run Tests

```bash
make test
```

### Run Linter

```bash
make lint
```

### Development Build (format + vet + test + build)

```bash
make dev-build
```

## Architecture

See [docs/architecture.md](docs/architecture.md) for detailed architecture documentation.

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

[MIT License](LICENSE)

## Acknowledgments

- Inspired by [kubecm](https://github.com/sunny0826/kubecm)
- Built with [Cobra](https://github.com/spf13/cobra) and [Survey](https://github.com/AlecAivazis/survey)
