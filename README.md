# chatbox

## Konfiguration

| Variable     | Default | Beschreibung                                                  |
|--------------|---------|---------------------------------------------------------------|
| `PORT`       | `3000`  | HTTP-Port                                                     |
| `API_KEY`    | `dummy` | Bearer-Key, den Open WebUI schickt (`Authorization: Bearer <key>`). In Produktion unbedingt setzen. |
| `LOG_LEVEL`  | `info`  | `debug`, `info`, `warn`, `error`. `debug` loggt alle Request-Header (`Authorization`/`Cookie` geschwärzt). |
| `LOG_FORMAT` | `text`  | `text` oder `json`                                            |

## Lokal testen

```sh
LOG_LEVEL=debug go run ./cmd/chatbox
curl -i localhost:3000/health          # {"status":"ok"}
curl -i -H "Authorization: Bearer dummy" localhost:3000/api/v1/policies
curl -i -X PUT -H "Authorization: Bearer dummy" -d '{"cpu_limit":"1"}' localhost:3000/api/v1/policies/dev
curl -i -H "Authorization: Bearer dummy" -H "X-User-Id: u1" localhost:3000/p/dev/files/list
```

## API

Die Schnittstelle ist in `openapi.json` beschrieben (Open Terminal 0.14.6 und der Orchestrator
Terminals 0.2.5). Daraus werden Server-Interface und Modelle generiert:

```sh
go generate ./internal/api
```

Generiert werden nur die Operationen, die der Orchestrator selbst behandelt (siehe
`include-tags` in `internal/api/oapi-codegen.yaml`). Alle übrigen Open-Terminal-Pfade laufen über den
Catch-all-Proxy zum Pod.

`chatbox healthcheck` prüft `/health` des laufenden Servers und wird im Docker-Image als `HEALTHCHECK` genutzt.

## Mit Open WebUI (Docker Compose)

```sh
cp .env.example .env            # CHATBOX_API_KEY setzen
docker compose up --build       # Open WebUI: http://localhost:8080
```

Ollama muss auf dem Host auf Port `11434` laufen. In Open WebUI unter Admin → Einstellungen den Terminal-Server
`http://chatbox:3000` mit Auth „Bearer“ und dem Wert von `CHATBOX_API_KEY` eintragen.
