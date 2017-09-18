# Copyright (C) The Arvados Authors. All rights reserved.
#
# SPDX-License-Identifier: AGPL-3.0

require 'fileutils'
require 'tmpdir'

# Commit log for "foo" repository in test.git.tar
# master is the main branch
# b1 is a branch off of master
# tag1 is a tag
#
# 1de84a8 * b1
# 077ba2a * master
# 4fe459a * tag1
# 31ce37f * foo

module GitTestHelper
  def self.included base
    base.setup do
      # Extract the test repository data into the default test
      # environment's Rails.configuration.git_repositories_dir. (We
      # don't use that config setting here, though: it doesn't seem
      # worth the risk of stepping on a real git repo root.)
      @tmpdir = Rails.root.join 'tmp', 'git'
      FileUtils.mkdir_p @tmpdir
      system("tar", "-xC", @tmpdir.to_s, "-f", "test/test.git.tar")
      Rails.configuration.git_repositories_dir = "#{@tmpdir}/test"

      # Initialize an empty internal git repo.
      intdir =
        Rails.configuration.git_internal_dir =
        Rails.root.join(@tmpdir, 'internal.git').to_s
      FileUtils.mkdir_p intdir
      IO.read("|git --git-dir #{intdir.shellescape} init")
      assert $?.success?
    end

    base.teardown do
      FileUtils.remove_entry @tmpdir, true
      FileUtils.remove_entry Commit.cache_dir_base, true
    end
  end

  def internal_tag tag
    IO.read "|git --git-dir #{Rails.configuration.git_internal_dir.shellescape} log --format=format:%H -n1 #{tag.shellescape}"
  end

  # Intercept fetch_remote_repository and fetch from a specified url
  # or local fixture instead of the remote url requested. fakeurl can
  # be a url (probably starting with file:///) or the name of a
  # fixture (as a symbol)
  def fetch_remote_from_local_repo url, fakeurl
    if fakeurl.is_a? Symbol
      fakeurl = 'file://' + repositories(fakeurl).server_path
    end
    Commit.expects(:fetch_remote_repository).once.with do |gitdir, giturl|
      if giturl == url
        Commit.unstub(:fetch_remote_repository)
        Commit.fetch_remote_repository gitdir, fakeurl
        true
      end
    end
  end
end
