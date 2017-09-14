# Copyright (C) The Arvados Authors. All rights reserved.
#
# SPDX-License-Identifier: AGPL-3.0

require 'test_helper'

class GroupTest < ActiveSupport::TestCase

  test "cannot set owner_uuid to object with existing ownership cycle" do
    set_user_from_auth :active_trustedclient

    # First make sure we have lots of permission on the bad group by
    # renaming it to "{current name} is mine all mine"
    g = groups(:bad_group_has_ownership_cycle_b)
    g.name += " is mine all mine"
    assert g.save, "active user should be able to modify group #{g.uuid}"

    # Use the group as the owner of a new object
    s = Specimen.
      create(owner_uuid: groups(:bad_group_has_ownership_cycle_b).uuid)
    assert s.valid?, "ownership should pass validation #{s.errors.messages}"
    assert_equal false, s.save, "should not save object with #{g.uuid} as owner"

    # Use the group as the new owner of an existing object
    s = specimens(:in_aproject)
    s.owner_uuid = groups(:bad_group_has_ownership_cycle_b).uuid
    assert s.valid?, "ownership should pass validation"
    assert_equal false, s.save, "should not save object with #{g.uuid} as owner"
  end

  test "cannot create a new ownership cycle" do
    set_user_from_auth :active_trustedclient

    g_foo = Group.create!(name: "foo")
    g_bar = Group.create!(name: "bar")

    g_foo.owner_uuid = g_bar.uuid
    assert g_foo.save, lambda { g_foo.errors.messages }
    g_bar.owner_uuid = g_foo.uuid
    assert g_bar.valid?, "ownership cycle should not prevent validation"
    assert_equal false, g_bar.save, "should not create an ownership loop"
    assert g_bar.errors.messages[:owner_uuid].join(" ").match(/ownership cycle/)
  end

  test "cannot create a single-object ownership cycle" do
    set_user_from_auth :active_trustedclient

    g_foo = Group.create!(name: "foo")
    assert g_foo.save

    # Ensure I have permission to manage this group even when its owner changes
    perm_link = Link.create!(tail_uuid: users(:active).uuid,
                            head_uuid: g_foo.uuid,
                            link_class: 'permission',
                            name: 'can_manage')
    assert perm_link.save

    g_foo.owner_uuid = g_foo.uuid
    assert_equal false, g_foo.save, "should not create an ownership loop"
    assert g_foo.errors.messages[:owner_uuid].join(" ").match(/ownership cycle/)
  end

  test "delete group hides contents" do
    set_user_from_auth :active_trustedclient

    g_foo = Group.create!(name: "foo")
    col = Collection.create!(owner_uuid: g_foo.uuid)

    assert Collection.readable_by(users(:active)).where(uuid: col.uuid).any?
    g_foo.update! is_trashed: true
    assert Collection.readable_by(users(:active)).where(uuid: col.uuid).empty?
    g_foo.update! is_trashed: false
    assert Collection.readable_by(users(:active)).where(uuid: col.uuid).any?
  end


  test "delete group propagates to subgroups" do
    set_user_from_auth :active_trustedclient

    g_foo = Group.create!(name: "foo")
    g_bar = Group.create!(name: "bar", owner_uuid: g_foo.uuid)
    col = Collection.create!(owner_uuid: g_bar.uuid)

    assert Group.readable_by(users(:active)).where(uuid: g_foo.uuid).any?
    assert Group.readable_by(users(:active)).where(uuid: g_bar.uuid).any?
    assert Collection.readable_by(users(:active)).where(uuid: col.uuid).any?

    g_foo.update! is_trashed: true
    assert Group.readable_by(users(:active)).where(uuid: g_foo.uuid).empty?
    assert Group.readable_by(users(:active)).where(uuid: g_bar.uuid).empty?
    assert Collection.readable_by(users(:active)).where(uuid: col.uuid).empty?

    set_user_from_auth :admin
    assert Group.readable_by(users(:active)).where(uuid: g_foo.uuid).empty?
    assert Group.readable_by(users(:active)).where(uuid: g_bar.uuid).empty?
    assert Collection.readable_by(users(:active)).where(uuid: col.uuid).empty?

    set_user_from_auth :active_trustedclient
    g_foo.update! is_trashed: false
    assert Group.readable_by(users(:active)).where(uuid: g_foo.uuid).any?
    assert Group.readable_by(users(:active)).where(uuid: g_bar.uuid).any?
    assert Collection.readable_by(users(:active)).where(uuid: col.uuid).any?
  end

end
