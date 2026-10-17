<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Third-Party Software Notices

The canonical notices for the desktop application and the service components
listed in `services/versions.json` are in
[the repository-root THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md).
This file is only a pointer, not a separate inventory.

Regenerate the canonical notice from the repository root after changing Go
modules, npm dependencies, Electron, or license-generation logic:

```bash
cd desktop && npm run licenses
```

See the canonical notice for scope, unresolved upstream attribution, and review
requirements. Distributions must include the canonical notice, not just this
pointer.
