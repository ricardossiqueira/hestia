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

## API local (comandos e administração de dispositivos)

`iot-gateway-admin.service` (ver `deploy/README.md` e ADR-008/ADR-013 em
`decisions.md`) é um serviço root separado, deliberadamente fora do sandbox
do gateway, porque `DeviceAdminService` precisa escrever
`/etc/mosquitto/*` e `gateway.yaml` e chamar `systemctl`. É também a única
borda pública de toda a API local (`DeviceService`/`GatewayService`
encaminhados por proxy ao processo sandboxed; `DeviceAdminService`
atendido ali mesmo). Limitações aceitas no MVP, mesma categoria das demais
desta página:

- Sem TLS: HTTP puro, só dentro da LAN confiável — nunca exponha essa porta
  além dela.
- Uma única credencial HTTP Basic Auth compartilhada, vinda de variáveis de
  ambiente — sem contas por operador.
- Sem CSRF nem modelo de sessão: `gateway-web` fala Connect JSON com
  `Authorization: Basic` explícito por requisição, não formulário HTML nem
  cookie — os vetores clássicos de CSRF (credencial ambiente do browser)
  não se aplicam. Uma UI HTML de admin existiu aqui até ADR-015; essa
  ressalva era dela.
- Sem limite de tentativas de login.

## A fazer antes de expor a VPS

- TLS ou túnel autenticado validado de ponta a ponta.
- Autenticação mútua gateway–VPS.
- Auditoria de comandos recebidos e publicados.
- Estratégia de rotação de credenciais.
