# SQLite REST API as a Unikraft unikernel

This repository is intentionally small: a Go REST API that persists data in a local SQLite database file, builds as a fully static Linux/amd64 binary, and can be packaged as a Unikraft unikernel for `urunc`.

## API

The service listens on `:8080` by default and stores data in `/data/app.db` by default.

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/healthz` | Health check with SQLite ping |
| `GET` | `/items` | List stored items |
| `POST` | `/items` | Create or replace an item with `{"key":"name","value":"data"}` |
| `GET` | `/items/{key}` | Fetch one item |
| `PUT` | `/items/{key}` | Create or replace one item with `{"value":"data"}` |
| `DELETE` | `/items/{key}` | Delete one item |

Example:

```bash
curl -sS -X POST http://127.0.0.1:8080/items \
  -H 'content-type: application/json' \
  -d '{"key":"hello","value":"unikraft"}'

curl -sS http://127.0.0.1:8080/items/hello
```

## Local development

```bash
make run
```

Configuration:

- `LISTEN_ADDR` or `-listen` sets the HTTP bind address.
- `PORT` is honored when the listener is still the default `:8080`.
- `DB_PATH` or `-db` sets the SQLite database path.

## Static build

The SQLite driver is `modernc.org/sqlite`, so the binary can be built with `CGO_ENABLED=0`.

```bash
make static
make verify-static
```

The output binary is `build/sqlite-api`. `make verify-static` fails if the binary contains a `PT_INTERP` program header or if `file(1)` does not report it as statically linked.

## Package for Unikraft / urunc

The `bunnyfile` includes:

- `build/sqlite-api` as `/sqlite-api`
- `rootfs/data` as `/data` for the SQLite file
- `rootfs/etc` as `/etc`

Build the unikernel image after placing an initrd-capable Unikraft elfloader at `build/elfloader_qemu-x86_64`:

```bash
make static
bunny build -f bunnyfile -t sqlite-api-unikernel
```

The boot command line starts:

```text
/sqlite-api -listen=:8080 -db=/data/app.db
```

`urunc`-specific deployment manifests are intentionally not included because networking and runtime class details vary by cluster setup.
