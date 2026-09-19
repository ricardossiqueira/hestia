# Fila persistente

## Propósito

A outbox SQLite retém dados aprovados para envio externo. Ela existe para sobreviver a reinícios do processo, falhas de energia e períodos sem conexão com a VPS.

Ela não substitui o MQTT local; MQTT entrega mensagens entre ESP32 e gateway, enquanto SQLite representa trabalho pendente de encaminhamento externo.

## Ciclo de uma mensagem

```text
recebida → validada → gravada como pendente → enviada → confirmada → removida
                                      └──── falha: nova tentativa com backoff
```

## Política inicial

- Gravação confirmada em disco antes de tentar transmitir à VPS.
- Ordem preservada por dispositivo quando isso for relevante.
- Retentativas exponenciais com limite superior de intervalo.
- Limite configurável de volume e idade dos registros.
- Ao atingir o limite, preservar comandos/resultados e eventos antes de descartar telemetria antiga; a política detalhada será implementada antes do uplink.

## Entrega

O objetivo é entrega **pelo menos uma vez**. A VPS também deverá deduplicar pelo `message_id`; tentar prometer exatamente uma vez adicionaria complexidade desnecessária nesta fase.
