# Project instructions

## Deploy after backend or web changes

Every change to Go code or files under `web/` requires running `deploy/install.sh` from the project root after completing the changes. This rebuilds and installs pircd and restarts its systemd user service so the changes take effect. Run applicable tests before deploying, and report any deployment failure.
