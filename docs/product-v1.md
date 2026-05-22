# readperfect V1

## Product rules

- A `book` is created and managed by a single author in V1.
- A book has at most one author owner in V1.
- Authors create the book workspace directly.
- Reviewers cannot create books or start reviews independently in V1.
- Reviewers participate only after an author invitation.
- Authentication supports both email/password and Google sign-in.
- Reviewer notes stay in draft until the reviewer explicitly submits them.
- Reviewer feedback is private between reviewer and author unless the author later opens visibility.

## Primary flows

### Author

1. Sign in with email/password or Google
2. Add a book by title and author
3. Optionally enrich it with metadata from a free books API
4. Add custom reader questions
5. Invite readers by email or invite link
6. Read submitted feedback by reviewer, page, and chapter

### Reviewer

1. Sign in with email or open an invite link
2. Open a book workspace
3. Add page or chapter notes
4. Save drafts while reading
5. Submit feedback deliberately when ready

## Homepage intent

The homepage should communicate:

- thoughtful early reader feedback
- author-led invitation flow
- private and deliberate submission flow
- warm literary editorial design

## Current implementation

- author signup and login with email/password
- Google OAuth plumbing with `.env` configuration
- secure cookie sessions
- protected author dashboard
- create-book flow with validation
- author book listing on dashboard
- per-book workspace page for authors
- author question creation
- invitation record creation with copyable invite links
- invite landing and acceptance flow with email-match enforcement
- reviewer workspace with draft submission creation
- page-note and chapter-note draft entry creation
- reviewer draft submission
- author view of submitted reviewer feedback
- note-level author reaction using `Insightful`
- single author comment on submitted notes
- reviewer visibility of author responses on submitted feedback
