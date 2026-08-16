# Cherri build patch

`cherri-v2.3.0.patch` modifies Cherri v2.3.0 solely as an optional maintainer
tool for compiling `ThingsIndex Helper.cherri`. Cherri is copyright its
contributors and licensed under GPL-2.0-only. The patch is distributed under
the same licence.

Upstream: <https://github.com/electrikmilk/cherri>

The patch preserves the native nested dictionaries expected by Things App
Intent actions and includes property metadata required when reading the
created task identifier. Neither Cherri nor patched Cherri code is included in
the ThingsIndex binaries.
