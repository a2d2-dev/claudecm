#!/usr/bin/env bash
# Setup shell completion for claudecm

set -e

# Detect shell
SHELL_NAME=$(basename "$SHELL")

echo "Setting up claudecm completion for $SHELL_NAME..."
echo ""

case "$SHELL_NAME" in
    zsh)
        COMPLETION_DIR="${HOME}/.zsh/completions"
        mkdir -p "$COMPLETION_DIR"

        echo "Generating zsh completion script..."
        claudecm completion zsh > "$COMPLETION_DIR/_claudecm"

        echo ""
        echo "✓ Completion script installed to: $COMPLETION_DIR/_claudecm"
        echo ""
        echo "Add the following to your ~/.zshrc if not already present:"
        echo ""
        echo "  # Enable completion"
        echo "  autoload -U compinit; compinit"
        echo "  # Add completion directory to fpath"
        echo "  fpath=($COMPLETION_DIR \$fpath)"
        echo ""
        echo "Then run: source ~/.zshrc"
        ;;

    bash)
        if [[ "$OSTYPE" == "darwin"* ]]; then
            # macOS
            if command -v brew &> /dev/null; then
                COMPLETION_DIR="$(brew --prefix)/etc/bash_completion.d"
                mkdir -p "$COMPLETION_DIR"
                echo "Generating bash completion script..."
                claudecm completion bash > "$COMPLETION_DIR/claudecm"
                echo "✓ Completion script installed to: $COMPLETION_DIR/claudecm"
            else
                echo "Homebrew not found. Please install bash-completion via Homebrew first."
                exit 1
            fi
        else
            # Linux
            COMPLETION_DIR="/etc/bash_completion.d"
            if [[ ! -w "$COMPLETION_DIR" ]]; then
                echo "Warning: $COMPLETION_DIR is not writable. Using ~/.bash_completion.d instead."
                COMPLETION_DIR="$HOME/.bash_completion.d"
                mkdir -p "$COMPLETION_DIR"
            fi
            echo "Generating bash completion script..."
            claudecm completion bash > "$COMPLETION_DIR/claudecm"
            echo "✓ Completion script installed to: $COMPLETION_DIR/claudecm"
        fi
        echo ""
        echo "Restart your shell or run: source ~/.bashrc"
        ;;

    fish)
        COMPLETION_DIR="$HOME/.config/fish/completions"
        mkdir -p "$COMPLETION_DIR"

        echo "Generating fish completion script..."
        claudecm completion fish > "$COMPLETION_DIR/claudecm.fish"

        echo "✓ Completion script installed to: $COMPLETION_DIR/claudecm.fish"
        echo ""
        echo "Completion will be available in new fish sessions."
        ;;

    *)
        echo "Unsupported shell: $SHELL_NAME"
        echo "Supported shells: zsh, bash, fish"
        echo ""
        echo "You can manually generate completion with:"
        echo "  claudecm completion [bash|zsh|fish|powershell]"
        exit 1
        ;;
esac

echo ""
echo "Setup complete! You can now use tab completion with claudecm."
