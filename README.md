# traveldoor.ge short links

A small URL shortener for `go.traveldoor.ge`. One Go binary, a SQLite file for storage, and an htmx admin page. No separate database container.

- `GET /<slug>` redirects (302) to the saved destination and counts the click.
- `/admin` is a password-protected page to create, copy and delete links.
- `/` redirects to the main website. `/healthz` returns `ok`.

## Configuration

All settings are environment variables. See [.env.example](.env.example).

| Variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `BASE_URL` | yes | `http://localhost:8080` | Public address of the service, shown on every short link. No trailing slash. |
| `ADMIN_PASS` | yes | none | Password for `/admin`. The app does not start without it. Use letters and digits only; Docker Compose rewrites `$`. |
| `ADMIN_USER` | no | `admin` | User name for `/admin`. |
| `ROOT_REDIRECT` | no | `https://traveldoor.ge` in Compose | Where the bare domain redirects. |
| `PORT` | no | `8080` | Port the server listens on. |
| `DB_PATH` | no | `./links.db` (`/data/links.db` in Docker) | SQLite database file. |

## Run locally

Requires Go 1.26 or newer.

```bash
cp .env.example .env
```

Edit `.env`: set `BASE_URL=http://localhost:8080` and choose an `ADMIN_PASS`. Then start the app with the variables loaded:

```bash
set -a && . ./.env && set +a && go run .
```

Open http://localhost:8080/admin and sign in.

## Deploy on Dokploy

1. Create a **Compose** service and point it at this repository, branch `main`, compose path `./docker-compose.yml`.
2. In the **Environment** tab set:

   ```
   BASE_URL=https://go.traveldoor.ge
   ADMIN_USER=admin
   ADMIN_PASS=<long random letters and digits>
   ROOT_REDIRECT=https://traveldoor.ge
   ```

3. Deploy and check the container log shows `listening on :8080`.
4. Make sure the DNS A record `go.traveldoor.ge` points at the server. Only then, in the **Domains** tab, add host `go.traveldoor.ge`, service `shortlink`, container port `8080`, HTTPS on, Let's Encrypt.
5. Deploy again. Domain changes on a Compose service apply on redeploy.

Adding the domain before DNS resolves makes the certificate request fail, and Let's Encrypt rate-limits retries.

## Data and backups

Links and clicks live in one SQLite file, `/data/links.db`, in the Docker volume `shortlink-data`. Redeploys keep it; deleting the volume loses every link.

Consistent backup, run on the server (find the full volume name with `docker volume ls | grep shortlink-data`):

```bash
docker run --rm -v VOLUME_NAME:/data -v /root/shortlink-backups:/b alpine sh -c "apk add -q sqlite && sqlite3 /data/links.db \".backup /b/links-$(date +%F).db\""
```

## Notes

- Slugs are case-sensitive and may use letters, digits, `-` and `_`. `admin`, `api`, `static` and `healthz` are reserved.
- Random slugs are 6 characters from a 62-character alphabet.
- Only `http://` and `https://` destinations are accepted.
- The admin page lists the 500 most recent links.
- Click country is read from Cloudflare's `CF-IPCountry` header and stays empty when the domain is not proxied by Cloudflare.
