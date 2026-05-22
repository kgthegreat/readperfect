#!/bin/bash
set -euo pipefail

if [ ! -f "deploy.config" ]; then
    echo "deploy.config not found"
    exit 1
fi

source deploy.config

echo "Setting up systemd service on server..."

cat > /tmp/${SERVICE_NAME}.service << SERVICEEOF
[Unit]
Description=readperfect
After=network.target

[Service]
Type=simple
User=$SSH_USER
Group=$SSH_USER
WorkingDirectory=$APP_HOME
ExecStart=$APP_HOME/$BINARY_NAME
Restart=always
RestartSec=10
StandardOutput=journal
StandardError=journal
SyslogIdentifier=$SERVICE_NAME

NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=$APP_HOME

[Install]
WantedBy=multi-user.target
SERVICEEOF

scp -P "$SSH_PORT" /tmp/${SERVICE_NAME}.service "$SSH_USER@$SSH_HOST:/tmp/"

ssh -t -p "$SSH_PORT" "$SSH_USER@$SSH_HOST" "
    sudo mv /tmp/${SERVICE_NAME}.service /etc/systemd/system/${SERVICE_NAME}.service && \
    sudo systemctl daemon-reload && \
    sudo systemctl enable ${SERVICE_NAME} && \
    sudo systemctl start ${SERVICE_NAME} && \
    sleep 2 && \
    sudo systemctl status ${SERVICE_NAME} --no-pager | head -n 15
"

rm /tmp/${SERVICE_NAME}.service

echo "Systemd service configured"
