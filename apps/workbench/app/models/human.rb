# Copyright (C) The Arvados Authors. All rights reserved.
#
# SPDX-License-Identifier: AGPL-3.0

class Human < ArvadosBase
  def self.goes_in_projects?
    true
  end
end
