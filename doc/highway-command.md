# `highway` Command Usage

`highway` is an `rsync`-like HTTP synchronization client for `superhighway-server`. It recursively uploads or downloads files between a local `.../fs/` directory and a remote server, transferring only files that are missing or have changed byte size (unless `-all` is set).

## Synopsis

```bash
go run ./cmd/highway -fs <dir/fs/> -url 'http://<user>:<password>@<host>:<port>/' -c <download|upload> [-all] [-delete] [-v]
```

## Flags

| Flag | Required | Default | Description |
| :--- | :--- | :--- | :--- |
| `-fs` | Yes | `./fs/` | Local filesystem directory to sync. **Must** end with `/fs/` (e.g., `/tmp/fs/` or `./fs/`). |
| `-url` | Yes | *(none)* | Server URL with Basic Auth credentials (e.g., `'http://strick:pw123@pizga.net:1908/'`). |
| `-c` | Yes | *(none)* | Sync direction: `download` (remote $\to$ local) or `upload` (local $\to$ remote). |
| `-all` | No | `false` | Force all files to be transferred and overwritten, even if byte sizes match. |
| `-delete` | No | `false` | Delete files and directories in the destination that do not exist in the source. |
| `-v` | No | `false` | Log every HTTP request (`<METHOD> <URL>`) sent to the server to `stderr`. |

## Examples

### Download changed files from server to `/tmp/fs/`
```bash
go run ./cmd/highway -fs /tmp/fs/ -url 'http://strick:pw123@pizga.net:1908/' -c download
```

### Upload changed files from `/tmp/fs/` to server
```bash
go run ./cmd/highway -fs /tmp/fs/ -url 'http://strick:pw123@pizga.net:1908/' -c upload
```

### Mirror local `/tmp/fs/` to server (overwrite all, delete extra remote files, verbose HTTP log)
```bash
go run ./cmd/highway -fs /tmp/fs/ -url 'http://strick:pw123@pizga.net:1908/' -c upload -all -delete -v
```

### Mirror server to local `/tmp/fs/` (delete extra local files, verbose HTTP log)
```bash
go run ./cmd/highway -fs /tmp/fs/ -url 'http://strick:pw123@pizga.net:1908/' -c download -delete -v
```
