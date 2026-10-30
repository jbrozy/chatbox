# chatbox

## Konfiguration

| Variable     | Default | Beschreibung                                                  |
|--------------|---------|---------------------------------------------------------------|
| `PORT`       | `3000`  | HTTP-Port                                                     |
| `LOG_LEVEL`  | `info`  | `debug`, `info`, `warn`, `error`. `debug` loggt alle Request-Header (`Authorization`/`Cookie` geschwärzt). |
| `LOG_FORMAT` | `text`  | `text` oder `json`                                            |

## Lokal testen

```sh
LOG_LEVEL=debug go run ./cmd/chatbox
curl -i localhost:3000/health          # {"status":"ok"}
curl -i localhost:3000/ready
curl -i -H "X-User-Id: u1" localhost:3000/p/dev/files/list
```

`chatbox healthcheck` prüft `/health` des laufenden Servers und wird im Docker-Image als `HEALTHCHECK` genutzt.
