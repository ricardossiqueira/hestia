# Implantacao com systemd

`iot-gateway.service` executa o gateway como o usuario de sistema sem login
`iot-gateway`. A configuracao YAML fica em `/etc/iot-gateway/gateway.yaml` e
as credenciais MQTT ficam em `/etc/iot-gateway/environment`, separadas do
repositorio.

No Orange Pi, depois de atualizar o projeto, instale o binario e a unidade:

```bash
cd ~/iot-gateway
go build -o bin/iot-gateway ./cmd/gateway

id iot-gateway >/dev/null 2>&1 || \
  sudo useradd --system --user-group --no-create-home \
  --shell /usr/sbin/nologin iot-gateway

sudo install -d -m 0750 -o root -g iot-gateway /etc/iot-gateway
sudo install -d -m 0750 -o iot-gateway -g iot-gateway /var/lib/iot-gateway
sudo install -m 0755 bin/iot-gateway /usr/local/bin/iot-gateway
sudo install -m 0640 -o root -g iot-gateway \
  config/gateway.yaml /etc/iot-gateway/gateway.yaml
sudo install -m 0644 deploy/iot-gateway.service \
  /etc/systemd/system/iot-gateway.service
```

Create `/etc/iot-gateway/environment` with the names configured in `mqtt`.
For the current sample configuration:

```ini
MQTT_GATEWAY_USERNAME=gateway
MQTT_GATEWAY_PASSWORD=the-gateway-mqtt-password
```

Protect the environment file and enable the service at boot:

```bash
sudo chown root:iot-gateway /etc/iot-gateway/environment
sudo chmod 640 /etc/iot-gateway/environment
sudo systemctl daemon-reload
sudo systemctl enable --now iot-gateway.service
systemctl status iot-gateway.service
```

Follow operational logs with:

```bash
journalctl -u iot-gateway.service -f
```
