# Copyright (C) The Arvados Authors. All rights reserved.
#
# SPDX-License-Identifier: AGPL-3.0

module UpdatePriority
  extend CurrentApiClient

  # Clean up after races.
  #
  # If container priority>0 but there are no committed container
  # requests for it, reset priority to 0.
  #
  # If container priority=0 but there are committed container requests
  # for it with priority>0, update priority.
  #
  # Normally, update_priority is a no-op if another thread/process is
  # already updating. Test cases that need to check priorities after
  # updating can force a (possibly overlapping) update in the current
  # thread/transaction by setting the "nolock" flag. See #14878.
  def self.update_priority(nolock: false)
    if !File.owned?(Rails.root.join('tmp'))
      Rails.logger.warn("UpdatePriority: not owner of #{Rails.root}/tmp, skipping")
      return
    end
    lockfile = Rails.root.join('tmp', 'update_priority.lock')
    File.open(lockfile, File::RDWR|File::CREAT, 0600) do |f|
      return unless nolock || f.flock(File::LOCK_NB|File::LOCK_EX)

      # priority>0 but should be 0:
      ActiveRecord::Base.connection.
        exec_query("UPDATE containers AS c SET priority=0 WHERE state IN ('Queued', 'Locked', 'Running') AND priority>0 AND uuid NOT IN (SELECT container_uuid FROM container_requests WHERE priority>0 AND state='Committed');", 'UpdatePriority')

      # priority==0 but should be >0:
      act_as_system_user do
        Container.
          joins("JOIN container_requests ON container_requests.container_uuid=containers.uuid AND container_requests.state=#{ActiveRecord::Base.connection.quote(ContainerRequest::Committed)} AND container_requests.priority>0").
          where('containers.state IN (?) AND containers.priority=0 AND container_requests.uuid IS NOT NULL',
                [Container::Queued, Container::Locked, Container::Running]).
          map(&:update_priority!)
      end
    end
  end

  def self.run_update_thread
    need = false
    Rails.cache.fetch('UpdatePriority', expires_in: 5.seconds) do
      need = true
    end
    return if !need

    Thread.new do
      Thread.current.abort_on_exception = false
      begin
        update_priority
      rescue => e
        Rails.logger.error "#{e.class}: #{e}\n#{e.backtrace.join("\n\t")}"
      ensure
        # Rails 5.1+ makes test threads share a database connection, so we can't
        # close a connection shared with other threads.
        # https://github.com/rails/rails/commit/deba47799ff905f778e0c98a015789a1327d5087
        if Rails.env != "test"
          ActiveRecord::Base.connection.close
        end
      end
    end
  end
end
