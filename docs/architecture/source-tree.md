# Source Tree

```plaintext
claudecm/
├── cmd/                        # Cobra command definitions
│   ├── root.go                 # Root command and global flags
│   ├── add.go                  # Add profile command
│   ├── list.go                 # List profiles command
│   ├── switch.go               # Switch active profile
│   ├── export.go               # Export environment vars
│   ├── delete.go               # Delete profile command
│   └── edit.go                 # Edit profile command
├── internal/                   # Internal packages (not importable)
│   ├── config/                 # Config management business logic
│   │   ├── manager.go          # Config manager implementation
│   │   ├── profile.go          # Profile model and methods
│   │   ├── state.go            # State model and methods
│   │   └── validator.go        # Validation logic
│   ├── storage/                # Storage layer
│   │   ├── filesystem.go       # File I/O operations
│   │   ├── yaml.go             # YAML serialization
│   │   └── paths.go            # Path construction helpers
│   ├── ui/                     # Interactive UI components
│   │   ├── prompt.go           # Survey-based prompts
│   │   ├── selector.go         # Profile selection UI
│   │   └── confirm.go          # Confirmation dialogs
│   └── export/                 # Export format handlers
│       ├── shell.go            # Shell export format
│       ├── json.go             # JSON export format
│       └── envfile.go          # .env file format
├── pkg/                        # Public packages (importable)
│   └── version/                # Version information
│       └── version.go
├── scripts/                    # Build and development scripts
│   ├── build.sh                # Local build script
│   ├── install.sh              # Local installation
│   └── test.sh                 # Run all tests
├── docs/                       # Documentation
│   ├── project-brief.md
│   ├── architecture.md
│   └── architecture/           # Architecture details
│       ├── coding-standards.md
│       ├── tech-stack.md
│       └── source-tree.md
├── .github/                    # GitHub configuration
│   └── workflows/
│       ├── ci.yaml             # CI pipeline
│       └── release.yaml        # Release automation
├── .goreleaser.yaml            # GoReleaser configuration
├── .golangci.yaml              # Linter configuration
├── go.mod                      # Go module definition
├── go.sum                      # Dependency checksums
├── main.go                     # Application entry point
├── Makefile                    # Build commands
├── README.md                   # Project README
└── LICENSE                     # License file
```

## Directory Purposes

### cmd/
Cobra command definitions. Each file represents a CLI command.

### internal/
Private application code. Cannot be imported by other projects.

#### internal/config/
Business logic for configuration management, validation, and state.

#### internal/storage/
File system operations, YAML serialization, path management.

#### internal/ui/
Interactive terminal UI components using Survey.

#### internal/export/
Different export format handlers (shell, JSON, .env).

### pkg/
Public packages that could be imported by other projects (version info).

### scripts/
Build, test, and installation automation scripts.

### docs/
Project documentation including architecture and user guides.
