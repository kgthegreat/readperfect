#!/bin/bash
set -euo pipefail

if [ ! -f "deploy.config" ]; then
    echo "deploy.config not found"
    exit 1
fi

source deploy.config

echo "Setting up SSL on server..."
echo
echo "Make sure DNS is configured first"
echo "Check: dig $DOMAIN +short"
echo
read -p "DNS configured and propagated? (y/n) " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    exit 1
fi

ssh -t -p "$SSH_PORT" "$SSH_USER@$SSH_HOST" "sudo certbot --nginx -d $DOMAIN -d www.$DOMAIN --non-interactive --agree-tos --register-unsafely-without-email || sudo certbot --nginx -d $DOMAIN -d www.$DOMAIN"

echo "SSL configured on server"
