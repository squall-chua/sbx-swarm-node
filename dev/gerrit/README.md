# Local Gerrit for git-backed workspaces

This sets up a Gerrit server in Docker and points a git-backed workspace at it,
so you can test the `gerrit_change` publish strategy end to end.

**Use SSH, not HTTPS.** The node's HTTPS token auth sends the username
`x-access-token` (see `internal/git/credential.go`). Real Gerrit checks the
username against the account, so that token fails. SSH is the clean path.

An SSH keypair is already generated here: `id_gerrit` (private) and
`id_gerrit.pub` (public). Both are git-ignored.

## 1. Start Gerrit

    docker compose -f dev/gerrit/docker-compose.yml up -d

First boot takes a minute (it initializes the site). Wait until
http://localhost:8080 loads.

## 2. Become admin and add your SSH key

Gerrit runs in dev-auth mode. The first account becomes admin.

1. Open http://localhost:8080 and click **Sign in** → **Become**.
2. Register a new account. Pick a **username** (Gerrit prompts for one). Call it
   `admin`. This username goes in the SSH URL later.
3. Go to **Settings → SSH Keys**. Paste the contents of `id_gerrit.pub`:

       cat dev/gerrit/id_gerrit.pub

## 3. Create a project

The `gerrit_change` strategy squashes onto the target branch, so the target
branch must already have a commit. `--empty-commit` gives it one.

    ssh -p 29418 -i dev/gerrit/id_gerrit admin@localhost \
        gerrit create-project demo --empty-commit --branch master

Replace `admin` with the username you set in step 2. Test the connection:

    ssh -p 29418 -i dev/gerrit/id_gerrit admin@localhost gerrit version

## 4. Point a workspace at it

Add this to your node config YAML (see `config.local.yaml` for the file shape).
Use an **absolute** path for `ssh_key_path`.

    workspaces:
      - name: gerrit-demo
        # no host_path: the node auto-manages the clone base under data_dir
        git:
          provider: gerrit                                    # required — never auto-derived
          remote_url: ssh://admin@localhost:29418/demo        # username must match step 2
          ssh_key_path: /home/mwchua/sbx-swarm-node/dev/gerrit/id_gerrit
          default_branch: master
          allow_push: true                                    # required for gerrit_change

Host-key check defaults to `accept-new` (trust on first use). To pin it instead,
after Gerrit is up:

    ssh-keyscan -p 29418 localhost > dev/gerrit/known_hosts

and add `ssh_known_hosts_path: /home/mwchua/sbx-swarm-node/dev/gerrit/known_hosts`.

## 5. Provision and publish

Start the node with the config above (`backend: sdk` for a real sandbox).

1. Create a sandbox with `clone: true`, one workspace mount `gerrit-demo`, and a
   `branch` (e.g. `feature`).
2. Do work and commit inside the sandbox on that branch.
3. Publish. The source branch is the sandbox's own HEAD — you only pass the
   target:

       PublishWorkRequest{ id: <sandbox-id>, strategy: "gerrit_change", target: "master" }

The response `change_id` and `delivery_url` (the `https://.../c/demo/+/<n>` line)
point at the new Gerrit change. Re-publishing the same workspace/source/target
adds a new patchset to the same change (deterministic Change-Id, ADR-0021).

## Tear down

    docker compose -f dev/gerrit/docker-compose.yml down        # keep data
    docker compose -f dev/gerrit/docker-compose.yml down -v     # wipe data
