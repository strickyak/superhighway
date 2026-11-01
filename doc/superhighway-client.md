# Superhighway Client Specification

## 1. Overview

A **Superhighway client** provides remote read/write filesystem access over HTTP to a Superhighway server. The server exposes its configured `.../fs/` directory as the root `/` of the virtual filesystem.

The protocol is designed to be stateless, human-debuggable, and minimal—requiring no XML, JSON, or multipart framing—so clients can be implemented easily in Go, Python, C, shell scripts, or resource-constrained environments.

---

## 2. Addressing, Authentication & URL Path Encoding

### 2.1 Base URL
A client is configured with a server base URL:
```text
http://<host>:<port>
```
(Optionally with a path prefix if reverse-proxied, though typically rooted at `/`.)

### 2.2 HTTP Basic Authentication
Every request must include an HTTP Basic Authentication header (`Authorization: Basic <base64(user:password)>`):
- **Username**: Any non-empty ASCII string up to 32 characters (`1 <= len(user) <= 32` after deleting any bytes outside `32..126`, excluding `:`). The server records this sanitized username in its stderr CSV audit log (`csv,<timestamp>,<ip>,<user>,<op>,<path>`) whenever a mutating operation (`PUT`, `RM`/`DELETE`, `MKDIR`/`MKCOL`, `RMDIR`) succeeds.
- **Password**: Must match the server's `--password` flag value.
- **Authentication Failure**: If credentials are missing, the username is invalid, or the password does not match, the server responds with **`418 I'm a teapot`** (instead of `401 Unauthorized`) and does not log the failed attempt.

### 2.3 Remote Path Normalization & Escaping
- Remote paths are POSIX-style paths starting with `/` (e.g., `/`, `/docs`, `/docs/hello.txt`).
- Path traversal segments (`..`) and NUL bytes (`\x00`) are forbidden and rejected by the server with `400 Bad Request`.
- **Percent-encoding is required on the wire**: Because filenames may contain spaces, `#`, `?`, `%`, and other reserved URL characters (e.g. `Filename might have spaces and #@%$%? funny chars!`), the client **must** RFC 3986 percent-encode path segments when constructing the HTTP request target:
  - In Go: `(&url.URL{Path: remotePath}).EscapedPath()` (or `url.PathEscape(segment)` joined by `/`).
  - Characters like `?` (`%3F`), `#` (`%23`), `%` (`%25`), and space (`%20`) MUST be escaped so they are not interpreted as HTTP query strings or URL fragments.

---

## 3. Wire Protocol Operations

All requests target `<base_url><escaped_remote_path>`.

### 3.1 `LS` — List Directory or File Metadata
- **Request**:
  ```http
  LS /path/to/target HTTP/1.1
  ```
  *(Aliases accepted by server: `LIST`, or `GET` when the target is a directory.)*
- **Success Response**: `200 OK`, `Content-Type: text/plain; charset=utf-8`
- **Response Body**:
  - An initial line with the base-10 integer count of entries (`<count>\n`), counting both files and directories.
  - `<count>` `\n`-terminated entry lines sorted lexicographically by entry name.
  - A final terminator line containing only `;` (`;\n`).
  ```text
  2
  2026-10-31 21:05:09 16 Filename might have spaces and #@%$%? funny chars!
  2026-10-31 21:05:09 4096 docs/
  ;
  ```
  - If the target is an empty directory, the response body is `0\n;\n`.
  - If the target is a single regular file, `<count>` is `1`, followed by that file's entry line and `;\n`.

### 3.2 `GET` — Read File Contents
- **Request**:
  ```http
  GET /path/to/file HTTP/1.1
  ```
  *(Optional `Range: bytes=<start>-<end>` header is supported via standard HTTP content serving.)*
- **Success Response**: `200 OK` (or `206 Partial Content`)
  - Headers include `Content-Length` (byte size) and `Last-Modified` (UTC HTTP-date).
  - Response body is the raw, unframed file bytes.
- **Directory Caveat**: Because `GET` on a directory returns its `LS` listing, a client implementing a strict `read_file` operation that wants to reject directories can either check `LS`/`HEAD` first or only call `GET` on known file paths.

### 3.3 `HEAD` — Stat File Headers
- **Request**:
  ```http
  HEAD /path/to/file HTTP/1.1
  ```
- **Success Response**: `200 OK` with `Content-Length` and `Last-Modified` headers and an empty body.

### 3.4 `PUT` — Create or Overwrite File
- **Request**:
  ```http
  PUT /path/to/file HTTP/1.1
  Content-Length: <n>

  <raw file bytes>
  ```
- **Behavior**:
  - Creates the file if it does not exist, or truncates and overwrites it if it does.
  - Parent directory must already exist (otherwise `404 Not Found`).
  - Target must not be an existing directory (otherwise `409 Conflict` or `400 Bad Request`).
- **Success Response**: `201 Created`

### 3.5 `MKDIR` — Create Directory
- **Request**:
  ```http
  MKDIR /path/to/dir HTTP/1.1
  ```
  *(Alias accepted by server: `MKCOL`)*
  - **Recursive Option (`mkdir -p`)**: Append `?p=1` (or `?parents=1`) to create intermediate parent directories automatically and succeed even if the directory already exists:
    ```http
    MKDIR /a/b/c?p=1 HTTP/1.1
    ```
- **Success Response**: `201 Created`
- **Failure**: `409 Conflict` if the directory already exists (when `?p=1` is not set); `404 Not Found` if parent directory does not exist (when `?p=1` is not set).

### 3.6 `RMDIR` — Remove Empty Directory
- **Request**:
  ```http
  RMDIR /path/to/dir HTTP/1.1
  ```
- **Behavior**:
  - Removes the target directory only if it is empty.
  - Fails with `400 Bad Request` if the target is a regular file, is the root `/`, or is not empty.
  - Fails with `404 Not Found` if the target does not exist.
- **Success Response**: `200 OK`

### 3.7 `RM` — Remove Regular File
- **Request**:
  ```http
  RM /path/to/file HTTP/1.1
  ```
  *(Alias accepted by server: `DELETE`)*
- **Behavior**:
  - Removes a non-directory file.
  - Fails with `400 Bad Request` if the target is a directory (clients must use `RMDIR` for directories).
  - Fails with `404 Not Found` if the target does not exist.
- **Success Response**: `200 OK`

### 3.8 `DU` — Recursive Disk Usage
- **Request**:
  ```http
  DU /path HTTP/1.1
  ```
- **Success Response**: `200 OK`, `Content-Type: text/plain; charset=utf-8`
- **Response Body**: A single `\n`-terminated line containing the base-10 sum of all non-directory file sizes (in bytes) rooted at `/path`:
  ```text
  37
  ```

### 3.9 `TIME` — Current Server Time
- **Request**:
  ```http
  TIME / HTTP/1.1
  ```
  *(Aliases accepted by server: `NOW`, `DATE`)*
- **Success Response**: `200 OK`, `Content-Type: text/plain; charset=utf-8`
- **Response Body**: A single `\n`-terminated line containing the friendly UTC timestamp (`YYYY-mm-dd HH:MM:SS`) followed by a space and Unix epoch seconds (accuracy ± a couple of seconds):
  ```text
  2026-10-31 21:44:02 1793483042
  ```

---

## 4. Parsing Specifications

### 4.1 Parsing `LS` Output
1. **Header & Footer Framing**:
   - Line 1: Base-10 integer `<count>` (`>= 0`), the total number of files and directories that follow.
   - Next `<count>` lines: One line per entry.
   - Final line: Must be exactly `;` (verifying the listing was not truncated).
2. **Entry Line Grammar** (with trailing `\n` or `\r\n` stripped):
```text
<YYYY-mm-dd> <HH:MM:SS> <size> <name>[/]
```
   - **Split on the first 3 ASCII spaces (`' '`)**:
     - Field 1 (`10` chars): Date in UTC (`YYYY-mm-dd`).
     - Field 2 (`8` chars): Time in UTC (`HH:MM:SS`).
     - Field 3 (`1+` digits): Decimal integer `<size>` in bytes (`int64`).
     - Field 4 (remainder of line): Entry name, with `/` appended if and only if the entry is a directory.
3. **Directory Detection**:
   - Since POSIX filenames cannot contain `/`, a trailing `/` on Field 4 unambiguously indicates `IsDir = true`.
   - Strip the trailing `/` to obtain the clean entry name.
4. **Why splitting on the first 3 spaces is exact**:
   - Even if a filename begins with a space, contains multiple consecutive spaces, or contains arbitrary punctuation (`#@%$%?`), everything after the space following `<size>` belongs verbatim to the filename.

### 4.2 Parsing `DU` Output
- Trim leading/trailing whitespace from the response body and parse as a base-10 `int64`.

### 4.3 Parsing `TIME` Output
- Trim trailing `\n` / `\r\n` and split on ASCII spaces (`' '`):
  - Field 1 (`YYYY-mm-dd`) + `" "` + Field 2 (`HH:MM:SS`): friendly UTC timestamp.
  - Field 3: base-10 Unix epoch seconds (`int64`).

---

## 5. Error Handling

When the server returns a non-`2xx` status code, the response body is a plain-text error message. Clients should map HTTP statuses to filesystem error conditions as follows:

| HTTP Status | Meaning |
| :--- | :--- |
| `200 OK` / `201 Created` | Operation succeeded |
| `400 Bad Request` | Invalid path (`..`), wrong type (`RM` on dir, `RMDIR` on file), or non-empty directory on `RMDIR` |
| `403 Forbidden` | Permission denied by underlying OS filesystem |
| `404 Not Found` | Path (or parent directory) does not exist (`fs.ErrNotExist`) |
| `405 Method Not Allowed` | Unknown HTTP method |
| `409 Conflict` | Target already exists (`fs.ErrExist`) or `PUT` target is a directory |
| `418 I'm a teapot` | Authentication failed (missing Basic Auth, invalid username, or wrong password) |

---

## 6. Reference Go Client Interface

### 6.1 Library API
```go
package client

import (
	"context"
	"io"
	"net/http"
	"time"
)

type Entry struct {
	Name    string    // Base name without trailing "/"
	Size    int64     // Size in bytes
	ModTime time.Time // UTC modification time (second precision)
	IsDir   bool      // True if line ended with "/"
}

type Client struct {
	BaseURL    string       // e.g. "http://localhost:1908"
	Username   string       // 1..32 printable ASCII chars
	Password   string       // Must match server --password
	HTTPClient *http.Client // Optional; defaults to http.DefaultClient
}

func New(baseURL, username, password string) *Client

func (c *Client) LS(ctx context.Context, remotePath string) ([]Entry, error)
func (c *Client) Get(ctx context.Context, remotePath string) (io.ReadCloser, error)
func (c *Client) Put(ctx context.Context, remotePath string, r io.Reader) error
func (c *Client) RM(ctx context.Context, remotePath string) error
func (c *Client) Mkdir(ctx context.Context, remotePath string, parents bool) error
func (c *Client) Rmdir(ctx context.Context, remotePath string) error
func (c *Client) DU(ctx context.Context, remotePath string) (int64, error)
func (c *Client) Time(ctx context.Context) (friendlyUTC string, unixSec int64, err error)
```
