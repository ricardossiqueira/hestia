# Implantação no Orange Pi

## Plataforma-alvo

- Orange Pi Zero 2W, 1 GB RAM.
- Sistema recomendado: Armbian minimal 64-bit baseado em Debian.
- Serviços iniciais: Mosquitto e `iot-gateway` via systemd.

## Premissas

- O Orange Pi possui IP estável ou hostname resolvível na rede local.
- ESP32 e Orange Pi estão na mesma rede Wi-Fi/LAN inicial.
- O armazenamento usa cartão microSD; minimizar escrita excessiva é importante.

## Diretrizes operacionais

- Executar o gateway como usuário de sistema sem login.
- Armazenar SQLite em `/var/lib/iot-gateway/`.
- Manter logs no journal do systemd e limitar retenção.
- Usar `Restart=on-failure` na unidade systemd.
- O diagnóstico HTTP escuta somente em `127.0.0.1:8080` por padrão. Para
  consultá-lo remotamente no futuro, use um túnel autenticado (por exemplo,
  WireGuard); não exponha essa porta na LAN.
- Fazer backup apenas de configuração e, quando necessário, da SQLite com o serviço parado ou usando backup consistente.

Os arquivos concretos de instalação serão adicionados quando o binário mínimo estiver funcional.
