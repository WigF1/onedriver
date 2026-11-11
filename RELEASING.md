# Releasing onedriver

Releases are built by GitHub Actions: pushing a version tag builds an RPM for
each supported Fedora version and publishes a GitHub release with the RPMs
attached. Other machines then install with a single `dnf install`, with no
repository clone or build tools needed.

## One-time setup

1. **Enable GitHub Actions** for the repository. GitHub turns workflows off in
   forks: open the repository's **Actions** tab and click
   *"I understand my workflows, go ahead and enable them"*.
2. Check the Fedora versions in `.github/workflows/release.yml`
   (`fedora: ["43", "44", "45"]`) cover the machines you install on
   (`rpm -E %fedora` shows a machine's version). Update the list as Fedora
   releases come and go.

## Making a release

1. **Pick the version**, e.g. `0.16.1`.
2. **Bump it** in these files (or run
   `pipx run bump-my-version bump --new-version 0.16.1`, which reads
   `.bumpversion.cfg`, then do the spec changelog by hand):
   - `onedriver.spec`: `Version:`, reset `Release:` to `1%{?dist}`, and add a
     `%changelog` entry at the top
   - `cmd/common/common.go`: `const version`
   - `pkg/resources/onedriver.1`: the `.TH` line
   - `.bumpversion.cfg`: `current_version`
3. Merge that change into `master` (through a pull request as usual).
4. **Tag and push:**

   ```bash
   git checkout master && git pull
   git tag -a v0.16.1 -m "onedriver 0.16.1"
   git push origin v0.16.1
   ```

5. Watch the **Release** workflow in the Actions tab (about 5–10 minutes). The
   tag must match `Version:` in `onedriver.spec`, otherwise the build stops
   with an error. When it's done, the release page has:
   - `onedriver-0.16.1-1.fc43.x86_64.rpm`, `…fc44…`, `…fc45…`
   - the matching `.src.rpm` files
   - `onedriver-0.16.1.tar.gz`: sources with vendored Go modules

   GitHub writes the release notes from the merged pull requests; edit them on
   the release page if you like.

To try a build without releasing, run the Release workflow by hand
(**Actions → Release → Run workflow**) and leave the tag empty. The RPMs are
then attached to the workflow run as artifacts and nothing is published.

**Adding a Fedora version to an existing release:** add it to the `fedora:`
list in `release.yml` and merge that, then run the Release workflow by hand
with the release's tag (e.g. `v0.16.0`). It builds that tag's sources for all
listed versions and adds the RPMs to the release (existing ones are replaced
by identical rebuilds). From the command line:
`gh workflow run release.yml -R WigF1/onedriver -f tag=v0.16.0`.

## Installing on another Fedora machine

```bash
rpm -E %fedora      # e.g. 43: pick the matching .fc43 RPM
sudo dnf install https://github.com/WigF1/onedriver/releases/download/v0.16.1/onedriver-0.16.1-1.fc43.x86_64.rpm
```

Then open **onedriver** from the applications menu, or see "Getting started" in
the README.

- If upstream onedriver from COPR is installed, remove it first:
  `sudo dnf remove onedriver && sudo dnf copr disable jstaf/onedriver`.
- If onedriver was installed with `sudo make install`, run
  `sudo make uninstall` in that checkout first: it installed the systemd unit
  in `/etc/systemd/user`, which would override the packaged one.

**Updating:** `dnf upgrade` doesn't see new releases, because they aren't in a
repository. Install the new release's RPM the same way: `dnf install` with the
new URL upgrades the installed version. Then restart the mount:
`systemctl --user restart onedriver@home-$USER-OneDrive.service`.

## Optional: automatic updates with COPR

For `dnf upgrade` to pick up new versions on every machine, build the package
in a [Fedora COPR](https://copr.fedorainfracloud.org/) repository as well.
This is optional and hasn't been tried with this fork yet.

1. Sign in to COPR with a Fedora account and create a project (e.g.
   `onedriver`), with the `fedora-*-x86_64` chroots you need.
2. Add a package: **Packages → New package**, source type **SCM**, clone URL
   `https://github.com/WigF1/onedriver`, build method **make srpm** (it uses
   `.copr/Makefile`, which runs `make srpm`).
3. For a build on every new tag, add COPR's webhook to the GitHub repository:
   copy the URL from the project's **Settings → Integrations** page into the
   repository's **Settings → Webhooks**.
4. On each machine:

   ```bash
   sudo dnf copr enable <your-copr-user>/onedriver
   sudo dnf install onedriver
   ```

## Building an RPM locally

```bash
sudo dnf install rpm-build rsync git make golang gcc pkgconf-pkg-config \
  webkit2gtk4.1-devel json-glib-devel
make rpm-local      # writes onedriver-VERSION-1.fcNN.x86_64.rpm here
```

`make rpm` does the same in a clean `mock` chroot (needs `mock` and membership
of the `mock` group).
