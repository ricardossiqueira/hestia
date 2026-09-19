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

Nao existe ainda transporte para a VPS, leitor da fila, lease, confirmacao ou
retentativa. Esses elementos pertencem ao uplink do Marco 5. A fila atual
prepara dados duraveis para esse consumidor futuro, sem depender dele.

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
