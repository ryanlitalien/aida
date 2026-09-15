# Three repos, one fresh cut

What is public, what stays private, and how the public repo was born. This
doubles as the transparency statement: the code ships; the person stays home.

## Public vs private

```mermaid
flowchart TD
    subgraph pub["PUBLIC"]
        AIDA["aida: the code<br/>engine · brain code · voice · loop<br/>(this repo)"]:::pub
        W2A["aida-android<br/>phone client"]:::soon
        W2B["aida-agents<br/>detached agent runner"]:::soon
        W2C["okf-wiki-template<br/>(conventions only, optional)"]:::soon
    end

    subgraph priv["PRIVATE: never published"]
        CFG[("aida-config → ~/.aida/<br/>sources, routes, roster:<br/>encodes whose life this is")]:::priv
        BRN[("aida-brain → ~/.aida/brain/<br/>everything ever said to it,<br/>auto-committed every query")]:::priv
        WIKI[("aida-wiki<br/>the personal archive itself")]:::priv
        LIFE[("life-log<br/>personal archive")]:::priv
    end

    AIDA -- "aida init bootstraps<br/>empty stand-ins" --> CFG
    AIDA -- "aida init creates an<br/>empty local-git brain" --> BRN
    W2A -. "second wave,<br/>after its own scrub" .- AIDA
    W2B -. "second wave" .- AIDA
    W2C -. "conventions extracted,<br/>zero content" .- WIKI

    classDef pub stroke-width:3px;
    classDef soon stroke-dasharray: 5 4;
    classDef priv stroke-width:1.5px;
```

The split is what makes open-sourcing tractable at all: **code, config, and
memory are three git repos.** The public repo is the code; the private repos
are the person. A new machine is two private clones and an env file; a new
*user* is `aida init`, which bootstraps an empty `~/.aida/` and an empty,
locally-git-initialized brain: point them at your own **private** remotes to
get the same backup story.

## The fresh cut

```mermaid
flowchart TD
    OLD["private development repo<br/>600+ commits, ~139 merged PRs<br/>(real IDs scrubbed along the way;<br/>full history stays unpublishable)"]:::priv
    OLD -- "renamed, stays writable<br/>(the historical record)" --> ARC(["aida-private"]):::priv
    OLD -- "scrubbed tree exported as<br/>ONE initial public commit<br/>+ HISTORY.md as the curated timeline" --> NEW["public aida repo<br/>granular, no-squash history<br/>accrues publicly from here"]:::pub
    NEW --> ORIG(["origin re-pointed on<br/>every dev machine"]):::pub

    classDef pub stroke-width:3px;
    classDef priv stroke-dasharray: 5 4;
```

Why a fresh start instead of publishing the rewritten history: on GitHub,
force-pushed-away commits remain reachable by SHA, in cached views, and
through `refs/pull/*`. With ~139 merged PRs, essentially every original
commit would survive even a perfect `git filter-repo` pass. Flipping the
original repo public can never be made safe, so it never happens. The public
repo starts from one clean commit; `HISTORY.md` ships as the curated,
sanitized record of what was built and when, and old PR numbers in docs point
at the renamed private repo instead.
