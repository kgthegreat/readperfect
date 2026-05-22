#!/bin/bash
set -euo pipefail

if [ ! -f "deploy.config" ]; then
    echo "deploy.config not found"
    exit 1
fi

source deploy.config

echo "Connecting to $SSH_USER@$SSH_HOST..."
ssh -p "$SSH_PORT" "$SSH_USER@$SSH_HOST"
