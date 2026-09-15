# Ecosystem map

What actually got built: one binary at the center, with config, memory, and
satellite projects as separate repos. Solid arrows are runtime reads/writes;
dashed arrows are planned or indirect. Public/private disposition per repo is
in `docs/diagrams/three-repo-cut.md`.

```mermaid
flowchart TD
    subgraph you["One person"]
        RY(["voice · CLI · phone · other agents"])
    end

    subgraph core["aida (this repo: the code)"]
        ENG["engine<br/>six-step pipeline"]
        VOICE["Jarvis / Aida<br/>voice layer"]
        LOOP["aida loop<br/>autonomous driver"]
        MCPS["MCP server + client"]
    end

    subgraph state["Your data (never in this repo)"]
        CFG[("aida-config<br/>library · routes · roster")]
        BRAIN[("aida-brain<br/>lessons · memories · tasks")]
        WIKI[("aida-wiki<br/>consolidated knowledge (OKF bundle)")]
    end

    subgraph sats["Satellites"]
        DROID["aida-android<br/>phone client (LMD)"]
        AGENTS["aida-agents<br/>detached agent runner"]
    end

    RY --> ENG
    RY --> VOICE
    DROID -- "/lmd/v1/* over tailnet" --> VOICE
    ENG <--> CFG
    ENG <--> BRAIN
    VOICE <--> BRAIN
    LOOP <--> BRAIN
    BRAIN -. "consolidate + decay<br/>(planned)" .-> WIKI
    MCPS <--> ENG
    AGENTS -- "runs" --> LOOP
```

Reading the picture:

- **Code, config, and memory are three git repos.** The code is public; the
  config repo encodes *whose* life this is (sources, routes, call-signs) and
  the brain repo holds everything ever said to it. Both stay private, and
  `aida init` bootstraps empty stand-ins for a new user.
- **Every surface converges on one brain.** Typed queries, voice turns, the
  phone, the loop, and other agents (via MCP) read and write the same lessons,
  memories, and tasks: that is what makes memory compound instead of
  fragmenting per tool.
- **The satellites are consumers of the contracts,** not forks: the Android
  client speaks the LMD wire protocol, and the agent runner drives the same
  task queue the loop drains.
