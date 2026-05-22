# readperfect

`readperfect` is a Go monolith with HTML templates for authors to invite trusted reviewers and collect deliberate book feedback.

## Run locally

```bash
cp .env.example .env
go run .
```

The app listens on `http://localhost:8080` by default.

## Authentication

V1 uses both:

- email + password
- Google OAuth

Google sign-in is enabled when these values are present in `.env`:

```bash
GOOGLE_CLIENT_ID=...
GOOGLE_CLIENT_SECRET=...
GOOGLE_REDIRECT_URL=http://localhost:8080/auth/google/callback
```

Optional `.env` values:

```bash
DATABASE_PATH=./readperfect.db
COOKIE_SECURE=false
BOOTSTRAP_ADMIN_EMAIL=you@example.com
```

The app reads `.env` automatically on startup if the file exists. Existing shell environment variables still take precedence.

## Current state

The initial scaffold includes:

- a Go HTTP server using the standard library
- embedded templates and static assets
- a homepage implementing the product and design direction for an author-led V1
- SQLite schema migration on startup
- email/password auth, Google OAuth wiring, and secure cookie sessions
- a protected author dashboard shell
- book creation and dashboard book listing for authors
- per-book author workspace with custom questions and invite-link generation
- invite acceptance and reviewer workspace with draft note creation
- reviewer submission and author-visible submitted feedback
- note-level author responses with `Insightful` reaction and a single comment

## Design direction

- calm editorial layout
- warm paper background
- serif headings with clean UI sans
- private, invitation-based feedback tone
