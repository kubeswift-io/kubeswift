# Maintainers

Maintainers review and merge pull requests, cut releases, triage issues and
security reports, and look after the project's direction.

| Name | GitHub | Area |
|---|---|---|
| William Rizzo | [@wrkode](https://github.com/wrkode) | Project lead, all areas |

## How decisions are made

- Day-to-day changes go through pull requests. A pull request needs approval
  from a maintainer and green CI, and is merged by squash merge.
- Changes to an API (a CRD field, a file format, a chart value) or to runtime
  behaviour start as an issue, so the design is agreed before the code.
- Maintainers aim for consensus. When there is none, the project lead decides
  and records why in the issue or pull request.
- Releases are cut by a maintainer following [docs/releases.md](docs/releases.md).

## Security

Vulnerabilities are reported privately through GitHub's
[private vulnerability reporting](https://github.com/kubeswift-io/kubeswift/security/advisories/new),
not in public issues. Maintainers acknowledge a report, fix it, and disclose it
with the release that carries the fix.

## Becoming a maintainer

Contributors who have made sustained, high-quality contributions (code,
reviews, docs, issue triage) over several months can be nominated by a
maintainer. The current maintainers decide by consensus, and the change is made
in a pull request to this file.

A maintainer who steps back moves to an emeritus list here, and can return by
the same process.
