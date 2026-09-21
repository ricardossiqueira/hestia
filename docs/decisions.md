# Decisões de arquitetura

| ID | Decisão | Motivo |
| --- | --- | --- |
| ADR-001 | Go no gateway | Binário único, baixo uso de recursos e boa concorrência. |
| ADR-002 | Mosquitto como broker local | Maduro, pequeno e separa broker da lógica de domínio. |
| ADR-003 | YAML como cadastro de dispositivos | Fácil de revisar e adequado ao conjunto inicial pequeno e estável. |
| ADR-004 | SQLite como outbox | Persistente, sem serviço adicional e apropriado ao Orange Pi. |
| ADR-005 | Sem regras locais no MVP | Mantém o escopo no transporte confiável de dados e comandos. |
| ADR-006 | WireGuard somente para a fase VPS | A rede externa não deve atrasar a validação do gateway local. |
| ADR-007 | Entrega pelo menos uma vez | Mais simples e robusta; deduplicação via IDs. |
| ADR-008 | UI de admin em serviço systemd separado, não no gateway | `iot-gateway.service` roda sandboxed (`ProtectSystem=strict`, `ReadWritePaths` só a outbox) de propósito; registrar dispositivo exige escrever `/etc/mosquitto/*` e `gateway.yaml` e chamar `systemctl`, o que o processo do gateway nunca deve poder fazer. Mesmo motivo do updater já ser um serviço root separado. |
| ADR-009 | API local dentro do processo `iot-gateway run`, não em serviço separado | Ao contrário do registro de dispositivo (ADR-008), publicar um comando só precisa da conexão MQTT que o gateway já mantém — abrir uma segunda conexão por requisição (como o CLI `publish-test-command` faz, aceitável por ser pontual) derrubaria repetidamente a sessão principal por colisão de `client_id`. Reaproveitar a conexão existente elimina o problema e não exige privilégio extra. Valia para `internal/commandapi` (removido) e vale do mesmo jeito para `internal/api`, que o substitui. |
| ADR-010 | Connect-RPC (`connectrpc.com/connect`) como transporte da API local, não gRPC puro | Uma única porta serve gRPC, gRPC-Web e HTTP/JSON — o último é o que faz `curl -d '{...}'` continuar funcionando sem cliente gRPC, sem perder o schema Protobuf nem a geração de stubs já usada em `api/proto`. |
| ADR-011 | Profile de device (`internal/deviceprofile`) valida `parameters` de comando contra Protobuf; MQTT continua JSON; device sem profile cai em fallback opaco | Sem schema, `{"on":"sim"}` publicava no MQTT sem erro e o firmware ignorava em silêncio. O profile é opt-in por dispositivo (`profile:` no YAML) para não obrigar toda a base existente a declarar um profile antes de continuar funcionando. |
| ADR-012 | `google.protobuf.Struct` em `PublishCommandRequest.parameters`, não `bytes *_json` como em `uplink.proto` | Em JSON, `bytes` vira base64 e quebraria `curl` e formulários simples — o ponto desta API. O custo é a precisão de número de `Struct` (decodifica como `double`); aceitável porque os parâmetros são validados e recodificados contra um schema Protobuf logo em seguida. Regra: parâmetro inteiro acima de 2^53 deve ser declarado `string` na mensagem do comando. |
