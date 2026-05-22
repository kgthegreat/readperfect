#!/bin/bash
set -euo pipefail

if [ ! -f "deploy.config" ]; then
    echo "deploy.config not found"
    exit 1
fi

source deploy.config

echo "readperfect Logs"
echo "================"
echo
echo "1) Application logs (live)"
echo "2) Application logs (last 100 lines)"
echo "3) Nginx access logs"
echo "4) Nginx error logs"
echo "5) Service status"
echo
read -p "Choose option: " option

case $option in
    1)
        ssh -t -p "$SSH_PORT" "$SSH_USER@$SSH_HOST" "sudo journalctl -u $SERVICE_NAME -f"
        ;;
    2)
        ssh -t -p "$SSH_PORT" "$SSH_USER@$SSH_HOST" "sudo journalctl -u $SERVICE_NAME -n 100 --no-pager"
        ;;
    3)
        ssh -t -p "$SSH_PORT" "$SSH_USER@$SSH_HOST" "sudo tail -f /var/log/nginx/access.log"
        ;;
    4)
        ssh -t -p "$SSH_PORT" "$SSH_USER@$SSH_HOST" "sudo tail -f /var/log/nginx/error.log"
        ;;
    5)
        ssh -t -p "$SSH_PORT" "$SSH_USER@$SSH_HOST" "sudo systemctl status $SERVICE_NAME"
        ;;
    *)
        echo "Invalid option"
        ;;
esac
