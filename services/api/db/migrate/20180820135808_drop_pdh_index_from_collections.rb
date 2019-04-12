# Copyright (C) The Arvados Authors. All rights reserved.
#
# SPDX-License-Identifier: AGPL-3.0

class DropPdhIndexFromCollections < ActiveRecord::Migration[4.2]
  def change
    remove_index :collections, column: :portable_data_hash
  end
end
