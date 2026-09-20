# Segurança

## Limites iniciais

- Mosquitto ficará acessível apenas na LAN confiável; não será exposto diretamente à internet.
- Cada ESP32 terá credenciais MQTT próprias assim que o primeiro teste simples estiver funcionando.
- ACLs do Mosquitto deverão limitar cada ESP32 aos seus próprios tópicos `devices/<id>/#`.
- O gateway terá uma identidade MQTT própria, com acesso de leitura/escrita aos tópicos declarados.
- O gateway só aceita comandos externos pelo uplink autenticado futuro e somente quando `commands_from_vps: true`.

## Segredos

- Não versionar senhas, certificados, chaves privadas ou arquivos `.env` reais.
- Usar permissões restritivas nos arquivos de configuração locais.
- As chaves WireGuard futuras pertencem ao sistema operacional e não ao repositório.

## UI de admin (registro de dispositivos)

`iot-gateway-admin.service` (ver `deploy/README.md` e ADR-008 em
`decisions.md`) é um serviço root separado, deliberadamente fora do sandbox
do gateway, porque precisa escrever `/etc/mosquitto/*` e `gateway.yaml` e
chamar `systemctl`. Limitações aceitas no MVP, mesma categoria das demais
desta página:

- Sem TLS: HTTP puro, só dentro da LAN confiável — nunca exponha essa porta
  além dela.
- Uma única credencial HTTP Basic Auth compartilhada, vinda de variáveis de
  ambiente — sem contas por operador.
- Sem proteção CSRF (um único operador confiável, sem modelo de sessão).
- Sem limite de tentativas de login.

## A fazer antes de expor a VPS

- TLS ou túnel autenticado validado de ponta a ponta.
- Autenticação mútua gateway–VPS.
- Auditoria de comandos recebidos e publicados.
- Estratégia de rotação de credenciais.
