# mail-router

HTTP service that delivers transactional email for api-core (hoanxu.com and
hoantien.work) through one or more email providers: SMTP, Resend, Brevo,
SendGrid and Amazon SES, alone or several in failover or weighted
round-robin.

api-core composes every email (templates, links, plain-text part) and posts
the finished message here. The mail-router picks the provider, moves to
another one when a provider is rate limited, over quota or down, and reports
an error class back so api-core's consumers keep their retry behaviour.
Provider keys live only in the mail-router's `.env`.

```text
 api-core (each server)                         mail-router (same server)
   feature services -> email.Mailer                POST /v1/send
     compose template, identity, validation  --->    auth, validation, idempotency
     email.MailRouter (HTTP client)                  Instrument(Router(Retry(RateLimit(provider))))
       MAIL_ROUTER_URL / MAIL_ROUTER_TOKEN             smtp | resend | brevo | sendgrid | ses | log | noop
```

## API

All endpoints answer JSON. `/v1/*` needs `Authorization: Bearer
$MAIL_ROUTER_TOKEN`.

### `POST /v1/send`

```json
{
  "from": {"name": "Hoàn Xu", "address": "noreply@hoanxu.com"},
  "to": [{"name": "Bạn", "address": "user@example.com"}],
  "reply_to": {"address": "support@hoanxu.com"},
  "subject": "Hoàn tiền thành công",
  "html": "<p>...</p>",
  "text": "...",
  "headers": {"List-Unsubscribe": "<https://hoanxu.com/u>"},
  "category": "cashback_paid",
  "tags": {"server": "hoanxu.com"},
  "idempotency_key": "cashback_paid:8f1c..."
}
```

* `to`, `subject` and `html` or `text` are required; at most 50 recipients.
  Unknown fields are rejected. The body is limited to 2 MB.
* `from` is optional. A missing address or name uses `EMAIL_FROM_ADDRESS`
  (else `EMAIL_FROM_EMAIL`, else `EMAIL_SMTP_USERNAME`) and
  `EMAIL_FROM_NAME`.
* Line breaks in the subject, addresses, headers or tags are rejected
  (header injection).
* `idempotency_key` (optional): a key that was already sent successfully
  within `MAIL_ROUTER_IDEMPOTENCY_TTL` (24 h) is not sent again; the first
  result comes back with `"duplicate": true`. While a key is being sent, a
  second request with it gets `409` (`temporary`). Failed sends are not
  remembered. The memory is per process; Resend and Brevo also receive the
  key and drop duplicates themselves.
* A send keeps running when the caller disconnects, bounded by
  `MAIL_ROUTER_SEND_TIMEOUT` (60 s), so a caller's retry finds it in the
  idempotency memory.

Success, `200`:

```json
{"provider": "brevo", "provider_message_id": "<...@smtp-relay.mailin.fr>"}
```

Failure:

```json
{"error": {"class": "quota_exceeded", "provider": "sendgrid",
           "message": "sendgrid: HTTP 401: Maximum credits exceeded (providers tried: brevo rate_limited, sendgrid quota_exceeded)"}}
```

| `class` | HTTP | Meaning | Caller |
| --- | --- | --- | --- |
| `rate_limited` | 429 | every provider tried was rate limited | retry later |
| `quota_exceeded` | 429 | quota used up / sending paused | retry later |
| `temporary` | 503 (409 while the same key is in flight) | network, timeout, 5xx | retry later |
| `invalid_recipient` | 422 | recipient rejected | do not retry |
| `permanent` | 502 from a provider, 400 for an invalid request | auth, unverified sender, bad message | do not retry |
| `unauthorized` | 401 | missing or wrong token | fix configuration |

`message` can contain the recipient address: callers must not log it
as is. The mail-router's own logs contain the class and category only.

### `GET /v1/stats`

```json
{"routing": "round_robin", "providers": ["brevo", "sendgrid", "smtp"],
 "counts": {"brevo ok": 120, "sendgrid quota_exceeded": 1, "sendgrid ok": 40}}
```

Counts are per process since start.

### `GET /healthz`

`{"status": "ok"}`, no token. The image's Docker health check runs
`/mail-router -healthcheck`.

## Configuration

Service keys:

| Key | Default | Meaning |
| --- | --- | --- |
| `MAIL_ROUTER_TOKEN` | none, required | shared secret, at least 32 characters (`openssl rand -hex 32`); the same value goes in api-core's `.env` |
| `MAIL_ROUTER_ADDR` | `:8080` | listen address |
| `MAIL_ROUTER_SEND_TIMEOUT` | `60s` | one message through every provider; api-core's `MAIL_ROUTER_TIMEOUT` (75 s) must be longer |
| `MAIL_ROUTER_IDEMPOTENCY_TTL` | `24h` | how long a sent `idempotency_key` is remembered |
| `CORE_NETWORK` | none, required by compose | api-core's Docker network |
| `MAIL_ROUTER_HOST_PORT` | `8090` | host port bound to 127.0.0.1 for checks |
| `EMAIL_FROM_NAME` | `Hoàn Xu` | default sender name |
| `EMAIL_FROM_ADDRESS` | `EMAIL_FROM_EMAIL`, else `EMAIL_SMTP_USERNAME` | default sender address |

Routing keys:

| Key | Default | Meaning |
| --- | --- | --- |
| `EMAIL_PROVIDER` | `smtp` | `smtp`, `resend`, `brevo`, `sendgrid`, `ses`, `log`, `noop`, or a comma-separated list with optional weights: `brevo:3,sendgrid:1,resend:1` |
| `EMAIL_ROUTING` | `failover` | `failover`: always start with the first provider of the list; `round_robin`: spread mail over the list by weight (see [Routing](#routing)) |
| `EMAIL_PROVIDER_FALLBACK` | empty | providers (one or a list) used, in order, only after every `EMAIL_PROVIDER` failed with a rate-limit / quota / temporary error |
| `EMAIL_PROVIDER_COOLDOWN` | `30m` | how long a provider that reported "quota exceeded" is tried only after the others |
| `EMAIL_RETRY_ATTEMPTS` | `1` | in-process attempts per provider (1 = no extra retry) |
| `EMAIL_RETRY_DELAY` | `500ms` | first backoff, doubled per retry |
| `EMAIL_RATE_LIMIT_PER_SECOND` | `0` | max sends started per second per provider (0 = off) |

Per provider (each provider reads its own keys):

| Provider | Keys |
| --- | --- |
| `smtp` | `EMAIL_SMTP_HOST` (`smtp.privateemail.com`), `EMAIL_SMTP_PORT` (`587` STARTTLS, `465` TLS), `EMAIL_SMTP_USERNAME`*, `EMAIL_SMTP_PASSWORD`*, `EMAIL_SMTP_TEXT_PART` (`false`; `true` sends multipart/alternative with the text part) |
| `resend` | `RESEND_API_KEY`*, `RESEND_API_URL` (`https://api.resend.com`), `RESEND_TIMEOUT` (`15s`), `RESEND_IDEMPOTENCY` (`true`) |
| `brevo` | `BREVO_API_KEY`* (`xkeysib-...`), `BREVO_API_URL` (`https://api.brevo.com`), `BREVO_TIMEOUT` (`15s`), `BREVO_IDEMPOTENCY` (`true`) |
| `sendgrid` | `SENDGRID_API_KEY`* (`SG....`, permission *Mail Send*), `SENDGRID_API_URL` (`https://api.sendgrid.com`; `https://api.eu.sendgrid.com` for an EU-pinned subuser), `SENDGRID_TIMEOUT` (`15s`) |
| `ses` | `AWS_REGION`* (or `SES_REGION`, or `AWS_DEFAULT_REGION`/shared config), credentials from the standard AWS chain (`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`, shared files, instance role), `SES_CONFIGURATION_SET` (optional) |
| `log` | none. Logs category, masked recipient (`v***@gmail.com`) and sizes, never bodies |
| `noop` | none. Drops every message |

`*` = required. When no `EMAIL_PROVIDER` entry can be built (a missing
key, an unknown name) the mail-router exits at startup with the reason, and
Docker restarts it; the log says what to fix. A provider in a list, or a
fallback, that cannot be built is reported
(`mail-router: configuration problem: provider disabled: ...` /
`fallback disabled: ...`) and left out; the others keep working.

See `.env.example` for every key.

## Routing

`internal/email/router.go` decides which provider sends each
message.

* **`EMAIL_ROUTING=failover`** (default). Every message starts with the
  first provider of `EMAIL_PROVIDER`. Weights are ignored.
* **`EMAIL_ROUTING=round_robin`**. Messages are spread over the
  `EMAIL_PROVIDER` list in proportion to the weights (smooth weighted round
  robin: `brevo:3,sendgrid:1,resend:1` sends brevo, sendgrid, brevo,
  resend, brevo, ...). Use it to add up the free daily quotas of several
  providers (for example Brevo 300/day, SendGrid and Resend 100/day each:
  weights `3,1,1`). The rotation is per mail-router process; after a restart
  it starts again at the first provider.

In both modes a send that fails with `ErrRateLimited`, `ErrQuotaExceeded`
(for example a Resend 429 `daily_quota_exceeded`, a Brevo 402
`not_enough_credits`, a SendGrid 401 `Maximum credits exceeded`, an SES
`LimitExceededException` / `SendingPausedException`) or `ErrTemporary` is
sent again, unchanged, through the remaining `EMAIL_PROVIDER` entries and
then through `EMAIL_PROVIDER_FALLBACK`, in order. The result names the
provider that accepted it, and the error of a message every provider
refused lists them all (`providers tried: brevo quota_exceeded, sendgrid
rate_limited`). Permanent and invalid-recipient errors are returned at
once: another provider would reject them too.

A provider that reported `ErrQuotaExceeded` is skipped for
`EMAIL_PROVIDER_COOLDOWN` (30 minutes by default): it is tried only after
every other provider, so a used-up daily quota costs one failed request per
cooldown instead of one per message. When every provider is cooling down
they are still tried, in the usual order.

A temporary error can be ambiguous, such as a timeout after the provider
already accepted the message. In that case moving on can duplicate the
email, the same at-least-once trade-off the consumers already make.

Example, in the mail-router `.env` on both servers:

```sh
EMAIL_PROVIDER=brevo:3,sendgrid:1,resend:1
EMAIL_ROUTING=round_robin
EMAIL_PROVIDER_FALLBACK=smtp            # last resort, never in the rotation
BREVO_API_KEY=xkeysib-...
SENDGRID_API_KEY=SG....
RESEND_API_KEY=re_...
```

Every provider in the list must be allowed to send from
`EMAIL_FROM_ADDRESS`, so the sending domain has to be authenticated (DKIM,
and SPF where the provider asks for it) at each of them; see below. Mail
from a provider whose domain is not authenticated is rejected
(`ErrPermanent`) or lands in spam.

## Setting up Brevo and SendGrid

Do every step on **both** servers, `hoanxu.com` and `hoantien.work`.

**Brevo**

1. *Senders, Domains & Dedicated IPs* → *Domains* → add `hoanxu.com`
   (and `hoantien.work` if that server sends from its own domain). Let
   Brevo add its records through Cloudflare (*Authorize DNS records*) or add
   them by hand with the proxy **off**: the `brevo-code:...` `TXT` on the
   root (a separate TXT record next to SPF, not part of it) and the DKIM
   `CNAME`s `brevo1._domainkey` and `brevo2._domainkey`. They do not touch
   Resend's `resend._domainkey` and `send` records. No SPF change is
   needed: Brevo uses its own bounce domain and DMARC passes through DKIM.
   Keep the existing single `_dmarc` record. Wait for *Authenticated*.
2. *SMTP & API* → *API keys* → create a key. Set `BREVO_API_KEY`.
3. A new Brevo account can be held *under validation* before it may send;
   until then sends fail as `ErrQuotaExceeded` and move to the next
   provider.

**SendGrid**

1. *Settings* → *Sender Authentication* → *Authenticate Your Domain* for
   `hoanxu.com` (and `hoantien.work` if needed), with automated security
   on. Add the `CNAME` records it shows to Cloudflare DNS with the proxy
   **off**, then *Verify*. No change to the root SPF record is needed.
2. *Settings* → *API Keys* → create a *Restricted Access* key with only
   *Mail Send* → *Full Access*. Set `SENDGRID_API_KEY`.
3. Turn off *Click Tracking* (*Settings* → *Tracking*) unless link
   rewriting through SendGrid's domain is wanted.

Then set the values above in the mail-router `.env`, run
`docker compose up -d`, check that `docker compose logs mail-router` shows
no `configuration problem`, and send a
low-risk email through each provider (temporarily `EMAIL_PROVIDER=brevo`,
then `sendgrid`) before turning on `round_robin`. Check the headers:
`dkim=pass`, `spf=pass`, `dmarc=pass`.

## Switching to Amazon SES

Do every step on **both** servers, `hoanxu.com` and `hoantien.work`. Each
server has its own `.env`, and the sender domain may differ per server.

1. **AWS account and region.** Use region `ap-southeast-1` (Singapore).
   Create an IAM user or role limited to `ses:SendEmail` (and
   `ses:SendRawEmail`), restricted to the verified identities. Keep its access
   key only in each server's `.env`. Never commit it.
2. **Verify the domain** in SES, under *Verified identities* → *Create
   identity* → *Domain* (`hoanxu.com`, and `hoantien.work` if that server
   sends from its own domain):
   * **DKIM:** choose Easy DKIM (RSA 2048). Add the three `CNAME` records
     SES shows to Cloudflare DNS with the proxy **off** (grey cloud).
   * **SPF:** add `include:amazonses.com` to the domain's existing SPF `TXT`
     record. Keep a single SPF record, for example
     `v=spf1 include:amazonses.com <existing includes> ~all`. For a
     custom MAIL FROM subdomain (recommended, for example `mail.hoanxu.com`),
     add its `MX` (`feedback-smtp.ap-southeast-1.amazonses.com`) and SPF
     `TXT` as SES shows.
   * **DMARC:** add or keep `_dmarc` `TXT`, for example
     `v=DMARC1; p=quarantine; rua=mailto:dmarc@hoanxu.com`. Start with
     `p=none` if no DMARC record exists yet.
   * Wait until SES shows the identity and DKIM as *Verified*.
3. **Leave the sandbox.** In *Account dashboard*, request production access
   (transactional mail, expected volume, how bounces and complaints are
   handled). The sandbox only delivers to verified addresses. Under
   `EMAIL_PROVIDER=ses` a sandbox rejection maps to `ErrInvalidRecipient`
   or `ErrPermanent`.
4. **Bounces and complaints (optional, recommended).** Create a
   configuration set, for example `hoanxu-transactional`, with an SNS event
   destination for Bounce/Complaint (an email subscription is enough to
   start). Set `SES_CONFIGURATION_SET`.
5. **Configure each server's mail-router `.env`:**

   ```sh
   EMAIL_PROVIDER=ses
   EMAIL_FROM_ADDRESS=noreply@hoanxu.com      # a verified identity
   EMAIL_FROM_NAME=Hoàn Xu
   AWS_REGION=ap-southeast-1
   AWS_ACCESS_KEY_ID=...
   AWS_SECRET_ACCESS_KEY=...
   SES_CONFIGURATION_SET=hoanxu-transactional  # optional
   # optional safety net during the switch:
   EMAIL_PROVIDER_FALLBACK=smtp                # keeps the EMAIL_SMTP_* settings
   ```

   Run `docker compose up -d` in the mail-router directory. The log must
   not show `configuration problem`.
6. **Verify.** Trigger a low-risk email (an admin account-correction preview
   or send to a test user) and check the SES sending statistics and the
   inbox (headers: `dkim=pass`, `spf=pass`, `dmarc=pass`).
7. Repeat steps 5 and 6 on the other server.

### Rollback

On the affected server, set `EMAIL_PROVIDER=smtp` in the mail-router `.env`
(or remove the key: `smtp` is the default) and run `docker compose up -d`.
No code change or api-core restart is needed.

## Deploying

The mail-router runs on **both** servers, next to api-core, from this
directory's `docker-compose.yml`. It joins api-core's Docker network
(`CORE_NETWORK`) under the alias `mail-router`, so api-core reaches it at
`http://mail-router:8080`. Its port is published on 127.0.0.1 only.

| Server | `CORE_NETWORK` |
| --- | --- |
| hoanxu.com (`/root/code/affiliate_core`) | `affiliate_core_api-core-network` |
| hoantien.work (`/home/vinitran/code/hoanxu-thong/affilate-core`) | `affilate-core_api-core-network` |

Check the name with `docker network ls | grep api-core-network` before the
first start.

Images: GitHub Actions (`.github/workflows/docker.yml`) runs the tests and
pushes `docker.io/<DOCKERHUB_USERNAME>/mail-router:latest` and `:<sha>` on
every push to `main`. The repository needs the `DOCKERHUB_USERNAME` and
`DOCKERHUB_TOKEN` secrets. api-core's Watchtower updates the
`mail-router` container when it is in its watch list.

### First rollout (per server)

The order matters: api-core sends nothing by itself any more, so the
mail-router must be running and api-core must have the token **before** the
new api-core image rolls out (Watchtower recreates api-core with the
environment of the running container).

1. Copy this directory's `docker-compose.yml` and `.env.example` to the
   server (for example `~/code/mail-router/`), `cp .env.example .env`.
2. Move the email provider keys from api-core's `.env` into the
   mail-router's `.env`: `EMAIL_PROVIDER*`, `EMAIL_FROM_*`,
   `EMAIL_SMTP_*`, `RESEND_*`, `SES_*`/`AWS_*` for SES. Set
   `MAIL_ROUTER_TOKEN` (`openssl rand -hex 32`) and `CORE_NETWORK`.
3. `docker compose pull && docker compose up -d`, then
   `curl -s 127.0.0.1:8090/healthz` and `docker compose logs mail-router`
   (it lists the providers; it must not show `configuration problem`).
4. Send a test message from the server:

   ```sh
   curl -s 127.0.0.1:8090/v1/send -H "Authorization: Bearer $TOKEN" \
     -H 'Content-Type: application/json' \
     -d '{"to":[{"address":"you@example.com"}],"subject":"mail-router test","text":"ok"}'
   ```

5. In api-core's `.env` add `MAIL_ROUTER_URL=http://mail-router:8080` and
   the same `MAIL_ROUTER_TOKEN`, then `docker compose up -d --no-deps api`
   (still the old image: it ignores the new keys).
6. Merge the api-core change. When Watchtower has rolled the new image out,
   check api-core's log for `email: configuration problem` (there must be
   none) and send a low-risk email (an admin account-correction preview).
   `GET /v1/stats` then counts it.
7. Remove the provider keys from api-core's `.env` once it is stable.

Repeat on the other server.

### Rollback

* Provider problem: change `EMAIL_PROVIDER` in the mail-router `.env` and
  `docker compose up -d`; api-core is not touched.
* mail-router problem: api-core's sends fail as `temporary` and the
  Kafka consumers / outbox retry them. Pin the previous image with
  `MAIL_ROUTER_IMAGE=docker.io/vinitran/mail-router:<sha>` and
  `docker compose up -d`.
* To go back to direct sending, redeploy the api-core image from before the
  mail-router change; its provider keys are still in api-core's `.env`
  until step 7.

## Development

```sh
go test -race ./...
MAIL_ROUTER_TOKEN=$(openssl rand -hex 32) EMAIL_PROVIDER=log \
  EMAIL_FROM_ADDRESS=noreply@hoanxu.com go run ./cmd/mail-router
```

`EMAIL_PROVIDER=log` logs each message (category, masked recipient, sizes)
instead of sending.

## Adding a provider

Example: Postmark.

1. Create `internal/email/provider/postmark.go`:

   ```go
   package provider

   import (
   	"context"
   	"fmt"
   	"net/http"

   	"mail-router/internal/email"
   )

   const PostmarkName = "postmark"

   func init() { email.Register(PostmarkName, newPostmarkFromEnv) }

   type Postmark struct {
   	token  string
   	client *http.Client
   }

   func newPostmarkFromEnv(env email.Env) (email.Sender, error) {
   	token := env.String("POSTMARK_SERVER_TOKEN", "")
   	if token == "" {
   		return nil, fmt.Errorf("POSTMARK_SERVER_TOKEN required")
   	}
   	return &Postmark{token: token, client: &http.Client{}}, nil
   }

   func (p *Postmark) Send(ctx context.Context, msg email.Message) (email.Result, error) {
   	result := email.Result{Provider: PostmarkName}
   	// Build the request from msg.From, msg.To, msg.Subject, msg.HTML, msg.Text,
   	// msg.ReplyTo, msg.Headers, msg.Category and msg.IdempotencyKey.
   	// Then map the outcome:
   	//   429                -> email.Classify(PostmarkName, email.ErrRateLimited, err)
   	//   quota / paused     -> email.ErrQuotaExceeded
   	//   bad recipient      -> email.ErrInvalidRecipient
   	//   5xx / network      -> email.ErrTemporary
   	//   everything else    -> email.ErrPermanent
   	// Set result.ProviderMessageID on success.
   	return result, nil
   }
   ```

2. Add a fake transport (an `httptest.Server` or a fake client interface) and
   run the shared contract in `provider/contract_test.go`:
   `runContract(t, func(t *testing.T) harness { ... })`. Add a request-shape
   test like `resend_test.go`.
3. Document the env keys in this file and in `.env.example`.
4. Deploy, then set `EMAIL_PROVIDER=postmark` (or use it as
   `EMAIL_PROVIDER_FALLBACK`).

api-core does not change: it only talks to the mail-router.
