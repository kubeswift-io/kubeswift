# KubeSwift Adopters

This is a list of organizations and users of KubeSwift. Adding yourself helps
the project understand who depends on it, which platforms and hardware matter,
and which features are actually in use.

If you are using KubeSwift, please add yourself by opening a pull request
against this file. If you would rather not be listed publicly but are willing
to be counted, open an issue and say so.

## Adopters

| Organization | Contact | Status | Description of Use |
|---|---|---|---|
| CLASTIX | @prometherion | testing | Part of our development environment, `baby-kmetal`, to test and offer testing environments of CLASTIX's kMetal product. |

<!--
| _Your organization_ | _@your-github-handle_ | _evaluating_ | _What you run on KubeSwift_ |
Add a row in alphabetical order by Organization. Keep it to one line.

  Organization  Company, institution, or project name. Use "(individual)" if
                you are adopting KubeSwift personally rather than on behalf of
                an organization.
  Contact       A GitHub handle, mailing list, or email address the project can
                reach for questions about your use. Do not list a personal
                email you do not want indexed by search engines.
  Status        One of:
                  evaluating  trying it out, not yet running workloads on it
                  testing     running non-production workloads
                  production  running production workloads
  Description   One sentence on what you run — the workload, and anything about
                your environment that shapes it (GPU passthrough, sandboxes,
                live migration, a particular CNI or storage backend). This is
                the most useful column: it tells maintainers which paths must
                not break.

Example:

| Example Corp | @example-handle | production | VM-backed CI runners and GPU inference sandboxes on bare metal. |
-->

## Why add yourself

Adopter entries are used to:

- prioritize work against real deployments rather than assumptions,
- decide which hardware, CNI, and storage combinations need to keep working,
- give new users evidence that the project is used elsewhere,
- reach you if a change is likely to affect your setup.

Entries are never used for marketing outreach.

## Removing yourself

If you stop using KubeSwift, open a pull request removing your row. No
explanation is required, though a note about why is welcome — an adopter
leaving is useful information.
