# Repository guide

Lark-Feishu-Chatbot is a Go service that streams model answers into Feishu Markdown cards. Read [README.md](README.md) (Chinese) or [README.en.md](README.en.md) (English) for user and deployment instructions, and [docs/technical-guide.md](docs/technical-guide.md) for architecture and operational details. The source and annotated configuration templates define the implemented behavior.

## Required documentation updates

Every repository change must include corresponding updates to [README.md](README.md), [README.en.md](README.en.md) and [docs/technical-guide.md](docs/technical-guide.md) in the same change set. This includes features, fixes, refactoring, tests, dependencies, configuration, deployment, documentation and agent instructions. Do not defer any of these documents to a later task or treat documentation synchronization as optional.

- `README.md` is the default Chinese user and deployment entry point; `README.en.md` is its English counterpart. Keep supported features, commands, setup, usage, development instructions and limitations current in both editions.
- `docs/technical-guide.md` is the detailed guide to the current implementation. Keep architecture, package responsibilities, message lifecycle, state/concurrency, model protocols, configuration, deployment, operations and verification details current.
- `AGENTS.md` defines contributor constraints and maintenance requirements. Update it whenever a change affects those rules.

Update the relevant sections rather than adding unrelated boilerplate. For changes without user-visible behavior, reflect the actual maintenance or verification implications in the development sections of both README editions and the technical guide. Before finishing, review all three documents against the final source and configuration templates, check their links and examples, and distinguish current behavior from historical records and checks that were not run.

## README style

- Write concise Simplified Chinese in `README.md` and matching English in `README.en.md`. Put reciprocal language links below the project title and center both titles with `<h1 align="center">`. Keep sections, claims, commands, configuration examples and alerts aligned. Use short paragraphs and lists; keep implementation details, full configuration references and verification records in the technical guide or annotated templates.
- Combine supported use cases, commands and model protocols under Features. Show project structure only as a directory diagram. Keep topology diagrams aligned with actual request and response paths.
- Highlight the seven feature labels in both editions with GitHub inline math, using `$\color{#BF8700}\textbf{Label}$` for golden-yellow bold text. Keep descriptions outside the math expression. This presentation depends on the reader's math-rendering support; do not replace it with remote images or CSS.
- Keep deployment steps minimal: list required settings, show image build and Compose startup, and briefly explain the public `/events` callback and optional model proxy. Link to the technical guide for platform setup details.
- Keep local development to a few lines: copy `.env.example` to `.env/.env.example`, fill in credentials, and use VS Code's Run and Debug GUI for `cmd/` entry points. Mention ignored `ignore/` here; omit environment-import scripts and extended tooling tutorials.
- Use generic, repository-relative examples. Keep license and provenance at the end without an introductory license label. Avoid repeating contributor rules; link to this guide and the technical guide.
- Use Markdown with repository-relative links and section anchors; omit external hyperlinks, images, badges and HTML styling except the centered title. Keep endpoint examples in inline code. Use GitHub alerts: `IMPORTANT` for the configuration list, `NOTE` for proxy and callback guidance, and `TIP` for private development files and document links.

## Code layout

- `cmd/lark-feishu-chatbot/`: service composition, logging, HTTP/TLS startup and graceful shutdown.
- `cmd/modelcheck/`: two-turn model connectivity, streaming and context diagnostic without a Feishu connection.
- `internal/config/`: direct environment parsing and validation.
- `internal/logging/`: shared JSON terminal/file output and command error reporting before log closure.
- `internal/model/`: model request formats, reusable HTTP transport and bounded SSE parsing.
- `internal/state/`: bounded conversation history, deduplication and user quotas.
- `internal/chat/`: admission, background generations, cancellation and serialized output.
- `internal/feishu/`: authenticated callbacks, message normalization, Markdown cards and platform transport.
- `docs/`: technical documentation and implementation records.

Keep entry points focused on composition and business behavior in the corresponding internal package. The existing `cmd/` and `internal/` structure is intentional; a `src/` directory is unnecessary.

## Behavior to preserve

Support private text messages and group text that mentions the configured bot. Replies within a topic share context; a new root message starts a separate topic. Group participants share that topic's history. The supported local commands are `/help` and `/clear`; there are no localized command aliases.

`/help` responds locally without calling the model, modifying conversation history or consuming a model question quota. `/clear` removes the topic's history and cancels its active generation before worker admission. Command replies and notices remain subject to bounded worker admission, so clearing can take effect even when its confirmation cannot be admitted.

Retain streaming Markdown cards, fenced code language labels, continuation cards, context expiry, request deadlines, file/stdout logging and user access limits. Actual Markdown and syntax presentation depends on the Feishu client.

Keep sessions, deduplication IDs, quotas and active-generation state bounded in process memory. Restart loses them, replicas do not share them, and full session capacity can evict an inactive topic before its TTL. Store only complete successful user/assistant pairs, trim complete pairs, reject concurrent generation within a topic, and prevent cleared or canceled generations from committing stale history. One worker owns each output session and serializes its card updates.

The model adapters support `chat-completions`, `responses` and `anthropic-messages`. Select the protocol explicitly; do not infer it from provider or model names. Append only the protocol resource path to `LLM_BASE_URL`; the configured URL includes any version prefix. Responses sends explicit history with `store:false`. Preserve completion/error validation and cancellation across all protocols. Tools, vision, audio, role selectors and billing are outside the implemented scope.

Despite the project name, the platform transport currently targets `https://open.feishu.cn`. A configurable international Lark API origin is not implemented; do not document it as supported.

## Configuration and deployment

Use [`.env.example`](.env.example) and [`docker-compose.yaml`](docker-compose.yaml) as the annotated configuration references. Keep their application environment names, order, sample values and individual English comments aligned whenever configuration changes.

Parse explicit environment settings directly into typed configuration. Do not add application YAML loading, hidden deployment defaults, compatibility aliases or multiple mapping layers. Optional empty values and explicit zero quotas retain their documented meanings. Preserve `ENABLE_LOG`, `USE_HTTPS`, `CERT_FILE`, `KEY_FILE` and `ACCESS_CONTROL_ENABLE` behavior.

For local development, copy the root `.env.example` to the ignored `.env/.env.example` and edit that local copy. Both VS Code launch configurations read `${workspaceFolder}/.env/.env.example` and use the repository root as `cwd`. The application does not load dotenv files itself.

Build the Docker image from the source clone, then deploy from a separate clean directory initially containing only the Compose file. Ensure the operator can manage that directory and Docker can create bind-mount directories there. Let Docker create missing `./logs` directories; do not require a manual `mkdir` step. Keep deployment data separate from the source clone.

Docker deployment uses literal values directly in Compose, without `.env` input or variable interpolation. Require building `lark-feishu-chatbot:local` with the Dockerfile before starting it with `docker compose up -d`. Keep each Compose service limited to `container_name`, `image`, `restart: always`, `ports`, `volumes` and `environment`; do not add a `build` entry or other runtime options to the minimal manifests. Preserve the writable log mount. Direct HTTPS requires a separately added read-only certificate mount. Keep the README deployment instructions concise and use Compose comments as the full configuration reference. Docker stdout/stderr retention follows daemon settings; the application log file requires host-managed rotation. Leave the Dockerfile without a `USER` instruction and Compose without a user override; the default root user supports ordinary root-owned log mounts. Rebuild the image and recreate containers after changing image runtime settings. Keep Dockerfile copy paths consistent with `.dockerignore`.

## Development and verification

Use the Go version declared in `go.mod` and the existing standard-library-first approach. Write source comments, technical documentation, configuration comments and diagnostic text in English; the default Chinese README is the documentation-language exception. Preserve Unicode handling with suitable non-Chinese fixtures such as emoji when needed.

Keep shared source, documentation, configuration examples and commit metadata free of personal filesystem paths, email addresses, account identifiers, private IPs/domains and machine-specific deployment details. Use repository-relative paths, `${workspaceFolder}`, generic placeholders and a project-neutral commit identity. Actual local credentials and settings belong only in Git-ignored files. Preserve required third-party copyright and license notices.

Format changed Go files with `gofmt`. Aim for source lines within 120 columns, counting a tab as four columns during review. Split long struct/map literals, calls and conditions at semantic boundaries; put fields of expanded keyed literals on separate lines and keep log keys beside their values. Split long string constants with concatenation while preserving their exact contents. `gofmt` does not wrap long expressions; the chosen layout must remain unchanged after another `gofmt` pass. Verify behavior changes with focused tests, then run the full local checks from the repository root:

```sh
go test ./...
go vet ./...
go build ./cmd/lark-feishu-chatbot ./cmd/modelcheck
```

Run `go test -race ./...` when a supported C toolchain and CGO are available. Report unavailable Docker, race or external integration checks accurately instead of treating them as passed. Prefer local HTTP/SSE fixtures and injected Feishu transports for automated tests.

`go run ./cmd/modelcheck` makes two real model requests. Use it only within the user's authorized endpoint/model scope and with appropriate credentials; do not substitute another model. Real Feishu acceptance is a separate integration activity. Never commit credentials, private environment files, logs, certificates or generated binaries, and do not print secret values during diagnostics.

Keep README feature claims and command descriptions consistent with the source. Preserve the standard Apache License 2.0 `LICENSE`, the project history and licensing scope documented in the technical guide, and dependency notices in `licenses/THIRD_PARTY_NOTICES`. Keep the README license label, Docker `/licenses/` distribution and `.dockerignore` inputs consistent with those files. The project license does not relicense third-party or upstream material; verify applicable rights and notice obligations when incorporating code or changing dependencies.
