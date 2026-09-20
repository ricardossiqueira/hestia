# Fila persistente

## Proposito

A outbox SQLite retem dados aprovados para envio externo. Ela sobrevive a
reinicios do processo, falhas de energia e periodos sem conexao com a VPS.

Ela nao substitui MQTT local: MQTT entrega mensagens entre dispositivos e
gateway, enquanto SQLite representa trabalho pendente de encaminhamento
externo.

## Implementacao atual

O gateway persiste `telemetry`, `state` e `event` somente quando o respectivo
campo `forwarding.*_to_vps` esta habilitado no YAML. `command_result` nao entra
na outbox nesta etapa porque nao possui uma autorizacao declarativa propria.

Nao existe ainda transporte para a VPS. A fila ja oferece ao futuro consumidor
um lease exclusivo e temporario, leitura ordenada, confirmacao explicita por
`message_id` e reagendamento de falhas. Esses recursos nao abrem conexao de
rede e nao dependem de WireGuard, gRPC ou de uma VPS disponivel.

## Garantias

- A gravacao confirmada no SQLite ocorre antes de um futuro envio externo.
- Cada `message_id` e unico na outbox. Uma duplicata nao duplica nem altera o
  registro original.
- A idade usa o horario local de enfileiramento, e nao o timestamp fornecido
  pelo dispositivo.
- A ordem de insercao e preservada por um identificador sequencial. A futura
  leitura devera usar essa ordem.
- A semantica planejada para o uplink e pelo menos uma vez. A VPS devera
  deduplicar por `gateway_id` e `message_id`.
- Um registro so e removido por uma confirmacao que contenha seu `message_id`
  e o token do lease que o reservou.
- Um lease expirado pode ser adquirido novamente; uma confirmacao atrasada do
  lease anterior nao remove o registro reacquirido.
- Uma falha de entrega libera o lease e define o proximo horario elegivel. A
  politica de backoff pertence ao cliente gRPC futuro.

## Limites e descarte

`storage.max_outbox_messages`, `storage.max_outbox_bytes` e
`storage.max_outbox_age` sao obrigatorios. Antes de cada insercao, registros
expirados sao removidos. Os limites contam mensagens e bytes de payload, nao o
tamanho fisico do arquivo SQLite.

Sob pressao, a ordem de descarte e deterministica: telemetria, depois estado,
e por ultimo evento. Uma mensagem nunca remove uma de prioridade maior. Se nao
houver uma vitima permitida, a mensagem recebida e descartada e o gateway
registra somente metadados do resultado, nunca o payload.

Falha ou saturacao da outbox nao bloqueia as rotas MQTT locais. Assim, uma
indisponibilidade de armazenamento ou da futura VPS nao interrompe o controle
na rede local.

## Contrato com a VPS

O protocolo gRPC/Protobuf de envio, confirmacao, retentativa e comando remoto
esta definido em [uplink-v1.md](uplink-v1.md). Aquele documento e uma
especificacao futura: esta etapa ainda nao abre conexao com a VPS nem altera o
funcionamento MQTT local.
