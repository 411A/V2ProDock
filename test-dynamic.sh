#!/bin/bash
set -e

# Setup xray
mkdir -p /tmp/v2test/xray
cd /tmp/v2test
curl -sL "https://github.com/XTLS/Xray-core/releases/latest/download/Xray-linux-64.zip" -o xray.zip
python3 -c "import zipfile; zipfile.ZipFile('xray.zip').extractall('xray')"
rm xray.zip
chmod +x xray/xray
ls -la xray/

# Build v2proxy
cd /mnt/x/Projects/V2RayInsideDocker/v2proxy
CGO_ENABLED=0 GOOS=linux go build -o /tmp/v2test/v2proxy .

# Test
cd /tmp/v2test
export SUBSCRIPTION_URL="http://192.168.1.87:27141/subscription.txt"
export HEALTH_CHECK_URL="https://api.ipify.org"
export XRAY_DIR="/tmp/v2test/xray"

./v2proxy 2>&1 &
PID=$!
sleep 15

echo ""
echo "=== Direct IP ==="
curl -s --max-time 5 https://api.ipify.org || echo "Failed"

echo ""
echo "=== Try SOCKS5 ports ==="
for port in 27019 27020 27021 27022 27023 27024 27025; do
    result=$(curl -s --max-time 5 --socks5 127.0.0.1:$port https://api.ipify.org 2>/dev/null)
    if [ -n "$result" ] && [ "$result" != "" ]; then
        echo "SOCKS5 port $port: $result"
        break
    fi
done

echo ""
echo "=== Try HTTP ports ==="
for port in 27020 27021 27022 27023 27024 27025 27026; do
    result=$(curl -s --max-time 5 --proxy http://127.0.0.1:$port https://api.ipify.org 2>/dev/null)
    if [ -n "$result" ] && [ "$result" != "" ]; then
        echo "HTTP port $port: $result"
        break
    fi
done

kill $PID 2>/dev/null
wait $PID 2>/dev/null
