# provider-mariadb
OpenEverest provider for MariaDB - uses community operator

## Installation

The provider chart is published as an OCI artifact to the GitHub Container
Registry on every release.

```bash
helm install provider-mariadb \
  oci://ghcr.io/openeverest/charts/provider-mariadb \
  --version 0.1.0 \
  --create-namespace
```

Upgrade to a newer chart version:

```bash
helm upgrade provider-mariadb \
  oci://ghcr.io/openeverest/charts/provider-mariadb \
  --version 0.1.0
```

Uninstall:

```bash
helm uninstall provider-mariadb
```

> Browse available versions on the
> [chart package page](https://github.com/openeverest/provider-mariadb/pkgs/container/charts%2Fprovider-mariadb).
