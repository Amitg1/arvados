# Copyright (C) The Arvados Authors. All rights reserved.
#
# SPDX-License-Identifier: AGPL-3.0

import pkg_resources

__version__ = pkg_resources.require('arvados_fuse')[0].version
