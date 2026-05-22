#!/bin/bash
set -euo pipefail

if [ ! -f "deploy.config" ]; then
    echo "deploy.config not found"
    exit 1
fi

source deploy.config

echo "Configuring Nginx on server..."

cat > /tmp/${NGINX_SITE_NAME}-nginx << 'NGINXEOF'
server {
    listen 80;
    server_name DOMAIN_PLACEHOLDER www.DOMAIN_PLACEHOLDER;

    location /.well-known/acme-challenge/ {
        root /var/www/html;
    }

    location / {
        proxy_pass http://localhost:PORT_PLACEHOLDER;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection 'upgrade';
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_cache_bypass $http_upgrade;

        proxy_connect_timeout 60s;
        proxy_send_timeout 60s;
        proxy_read_timeout 60s;
    }

    location /static/ {
        proxy_pass http://localhost:PORT_PLACEHOLDER;
        expires 1y;
        add_header Cache-Control "public, immutable";
    }
}
NGINXEOF

sed -i.bak "s/DOMAIN_PLACEHOLDER/$DOMAIN/g" /tmp/${NGINX_SITE_NAME}-nginx
sed -i.bak "s/PORT_PLACEHOLDER/$APP_PORT/g" /tmp/${NGINX_SITE_NAME}-nginx
rm /tmp/${NGINX_SITE_NAME}-nginx.bak

scp -P "$SSH_PORT" /tmp/${NGINX_SITE_NAME}-nginx "$SSH_USER@$SSH_HOST:/tmp/"

ssh -t -p "$SSH_PORT" "$SSH_USER@$SSH_HOST" "
    sudo mv /tmp/${NGINX_SITE_NAME}-nginx /etc/nginx/sites-available/${NGINX_SITE_NAME} && \
    sudo ln -sf /etc/nginx/sites-available/${NGINX_SITE_NAME} /etc/nginx/sites-enabled/ && \
    sudo nginx -t && \
    sudo systemctl reload nginx
"

rm /tmp/${NGINX_SITE_NAME}-nginx

echo "Nginx configured on server"
