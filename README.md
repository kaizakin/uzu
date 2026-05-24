# sqlite-api

A small Go HTTP API backed by SQLite.

## Status

The application is implemented and runnable locally. The remaining pending work is building and validating the unikernel image.

Done:
- Static Go binary build
- SQLite persistence
- CRUD HTTP API
- Local development run path
- Packaging files for unikernel/rootfs (`bunnyfile`, `Kraftfile`, `Dockerfile.rootfs`)

Pending:
- End-to-end unikernel build and boot verification

## Run locally

```bash
make static
make run
```

The service listens on `:8080` and uses `./app.db` when run locally.

## API

- `GET /healthz`
- `GET /items`
- `POST /items`
- `GET /items/{key}`
- `PUT /items/{key}`
- `DELETE /items/{key}`

Example:

```bash
curl -X POST http://localhost:8080/items \
  -H 'Content-Type: application/json' \
  -d '{"key":"hello","value":"world"}'
```

## Unikernel packaging

This repo includes:

- [`bunnyfile`](./bunnyfile)
- [`Kraftfile`](./Kraftfile)
- [`Dockerfile.rootfs`](./Dockerfile.rootfs)

The application is prepared for unikernel packaging, but the final build/boot path is still pending.
