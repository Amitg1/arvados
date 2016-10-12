require 'can_be_an_owner'

class User < ArvadosModel
  include HasUuid
  include KindAndEtag
  include CommonApiTemplate
  include CanBeAnOwner

  serialize :prefs, Hash
  has_many :api_client_authorizations
  validates(:username,
            format: {
              with: /^[A-Za-z][A-Za-z0-9]*$/,
              message: "must begin with a letter and contain only alphanumerics",
            },
            uniqueness: true,
            allow_nil: true)
  before_update :prevent_privilege_escalation
  before_update :prevent_inactive_admin
  before_update :verify_repositories_empty, :if => Proc.new { |user|
    user.username.nil? and user.username_changed?
  }
  before_create :check_auto_admin
  before_create :set_initial_username, :if => Proc.new { |user|
    user.username.nil? and user.email
  }
  after_create :add_system_group_permission_link
  after_create :auto_setup_new_user, :if => Proc.new { |user|
    Rails.configuration.auto_setup_new_users and
    (user.uuid != system_user_uuid) and
    (user.uuid != anonymous_user_uuid)
  }
  after_create :send_admin_notifications
  after_update :send_profile_created_notification
  after_update :sync_repository_names, :if => Proc.new { |user|
    (user.uuid != system_user_uuid) and
    user.username_changed? and
    (not user.username_was.nil?)
  }

  has_many :authorized_keys, :foreign_key => :authorized_user_uuid, :primary_key => :uuid
  has_many :repositories, foreign_key: :owner_uuid, primary_key: :uuid

  api_accessible :user, extend: :common do |t|
    t.add :email
    t.add :username
    t.add :full_name
    t.add :first_name
    t.add :last_name
    t.add :identity_url
    t.add :is_active
    t.add :is_admin
    t.add :is_invited
    t.add :prefs
    t.add :writable_by
  end

  ALL_PERMISSIONS = {read: true, write: true, manage: true}

  def full_name
    "#{first_name} #{last_name}".strip
  end

  def is_invited
    !!(self.is_active ||
       Rails.configuration.new_users_are_active ||
       self.groups_i_can(:read).select { |x| x.match(/-f+$/) }.first)
  end

  def groups_i_can(verb)
    my_groups = self.group_permissions.select { |uuid, mask| mask[verb] }.keys
    if verb == :read
      my_groups << anonymous_group_uuid
    end
    my_groups
  end

  def can?(actions)
    return true if is_admin
    actions.each do |action, target|
      unless target.nil?
        if target.respond_to? :uuid
          target_uuid = target.uuid
        else
          target_uuid = target
          target = ArvadosModel.find_by_uuid(target_uuid)
        end
      end
      next if target_uuid == self.uuid
      next if (group_permissions[target_uuid] and
               group_permissions[target_uuid][action])
      if target.respond_to? :owner_uuid
        next if target.owner_uuid == self.uuid
        next if (group_permissions[target.owner_uuid] and
                 group_permissions[target.owner_uuid][action])
      end
      sufficient_perms = case action
                         when :manage
                           ['can_manage']
                         when :write
                           ['can_manage', 'can_write']
                         when :read
                           ['can_manage', 'can_write', 'can_read']
                         else
                           # (Skip this kind of permission opportunity
                           # if action is an unknown permission type)
                         end
      if sufficient_perms
        # Check permission links with head_uuid pointing directly at
        # the target object. If target is a Group, this is redundant
        # and will fail except [a] if permission caching is broken or
        # [b] during a race condition, where a permission link has
        # *just* been added.
        if Link.where(link_class: 'permission',
                      name: sufficient_perms,
                      tail_uuid: groups_i_can(action) + [self.uuid],
                      head_uuid: target_uuid).any?
          next
        end
      end
      return false
    end
    true
  end

  def self.invalidate_permissions_cache(timestamp=nil)
    if Rails.configuration.async_permissions_update
      timestamp = DbCurrentTime::db_current_time.to_i if timestamp.nil?
      connection.execute "NOTIFY invalidate_permissions_cache, '#{timestamp}'"
    else
      Rails.cache.delete_matched(/^groups_for_user_/)
    end
  end

  # Return a hash of {group_uuid: perm_hash} where perm_hash[:read]
  # and perm_hash[:write] are true if this user can read and write
  # objects owned by group_uuid.
  #
  # The permission graph is built by repeatedly enumerating all
  # permission links reachable from self.uuid, and then calling
  # search_permissions
  def calculate_group_permissions
      permissions_from = {}
      todo = {self.uuid => true}
      done = {}
      # Build the equivalence class of permissions starting with
      # self.uuid. On each iteration of this loop, todo contains
      # the next set of uuids in the permission equivalence class
      # to evaluate.
      while !todo.empty?
        lookup_uuids = todo.keys
        lookup_uuids.each do |uuid| done[uuid] = true end
        todo = {}
        newgroups = []
        # include all groups owned by the current set of uuids.
        Group.where('owner_uuid in (?)', lookup_uuids).each do |group|
          newgroups << [group.owner_uuid, group.uuid, 'can_manage']
        end
        # add any permission links from the current lookup_uuids to a Group.
        Link.where('link_class = ? and tail_uuid in (?) and ' \
                   '(head_uuid like ? or (name = ? and head_uuid like ?))',
                   'permission',
                   lookup_uuids,
                   Group.uuid_like_pattern,
                   'can_manage',
                   User.uuid_like_pattern).each do |link|
          newgroups << [link.tail_uuid, link.head_uuid, link.name]
        end
        newgroups.each do |tail_uuid, head_uuid, perm_name|
          unless done.has_key? head_uuid
            todo[head_uuid] = true
          end
          link_permissions = {}
          case perm_name
          when 'can_read'
            link_permissions = {read:true}
          when 'can_write'
            link_permissions = {read:true,write:true}
          when 'can_manage'
            link_permissions = ALL_PERMISSIONS
          end
          permissions_from[tail_uuid] ||= {}
          permissions_from[tail_uuid][head_uuid] ||= {}
          link_permissions.each do |k,v|
            permissions_from[tail_uuid][head_uuid][k] ||= v
          end
        end
      end
      perms = search_permissions(self.uuid, permissions_from)
      Rails.cache.write "groups_for_user_#{self.uuid}", perms
      perms
  end

  # Return a hash of {group_uuid: perm_hash} where perm_hash[:read]
  # and perm_hash[:write] are true if this user can read and write
  # objects owned by group_uuid.
  def group_permissions
    r = Rails.cache.read "groups_for_user_#{self.uuid}"
    if r.nil?
      if Rails.configuration.async_permissions_update
        while r.nil?
          sleep(0.1)
          r = Rails.cache.read "groups_for_user_#{self.uuid}"
        end
      else
        r = calculate_group_permissions
      end
    end
    r
  end

  def self.setup(user, openid_prefix, repo_name=nil, vm_uuid=nil)
    return user.setup_repo_vm_links(repo_name, vm_uuid, openid_prefix)
  end

  # create links
  def setup_repo_vm_links(repo_name, vm_uuid, openid_prefix)
    oid_login_perm = create_oid_login_perm openid_prefix
    repo_perm = create_user_repo_link repo_name
    vm_login_perm = create_vm_login_permission_link vm_uuid, username
    group_perm = create_user_group_link

    return [oid_login_perm, repo_perm, vm_login_perm, group_perm, self].compact
  end

  # delete user signatures, login, repo, and vm perms, and mark as inactive
  def unsetup
    # delete oid_login_perms for this user
    Link.destroy_all(tail_uuid: self.email,
                     link_class: 'permission',
                     name: 'can_login')

    # delete repo_perms for this user
    Link.destroy_all(tail_uuid: self.uuid,
                     link_class: 'permission',
                     name: 'can_manage')

    # delete vm_login_perms for this user
    Link.destroy_all(tail_uuid: self.uuid,
                     link_class: 'permission',
                     name: 'can_login')

    # delete "All users" group read permissions for this user
    group = Group.where(name: 'All users').select do |g|
      g[:uuid].match(/-f+$/)
    end.first
    Link.destroy_all(tail_uuid: self.uuid,
                     head_uuid: group[:uuid],
                     link_class: 'permission',
                     name: 'can_read')

    # delete any signatures by this user
    Link.destroy_all(link_class: 'signature',
                     tail_uuid: self.uuid)

    # delete user preferences (including profile)
    self.prefs = {}

    # mark the user as inactive
    self.is_active = false
    self.save!
  end

  protected

  def ensure_ownership_path_leads_to_user
    true
  end

  def permission_to_update
    if username_changed?
      current_user.andand.is_admin
    else
      # users must be able to update themselves (even if they are
      # inactive) in order to create sessions
      self == current_user or super
    end
  end

  def permission_to_create
    current_user.andand.is_admin or
      (self == current_user and
       self.is_active == Rails.configuration.new_users_are_active)
  end

  def check_auto_admin
    return if self.uuid.end_with?('anonymouspublic')
    if (User.where("email = ?",self.email).where(:is_admin => true).count == 0 and
        Rails.configuration.auto_admin_user and self.email == Rails.configuration.auto_admin_user) or
       (User.where("uuid not like '%-000000000000000'").where(:is_admin => true).count == 0 and
        Rails.configuration.auto_admin_first_user)
      self.is_admin = true
      self.is_active = true
    end
  end

  def find_usable_username_from(basename)
    # If "basename" is a usable username, return that.
    # Otherwise, find a unique username "basenameN", where N is the
    # smallest integer greater than 1, and return that.
    # Return nil if a unique username can't be found after reasonable
    # searching.
    quoted_name = self.class.connection.quote_string(basename)
    next_username = basename
    next_suffix = 1
    while Rails.configuration.auto_setup_name_blacklist.include?(next_username)
      next_suffix += 1
      next_username = "%s%i" % [basename, next_suffix]
    end
    0.upto(6).each do |suffix_len|
      pattern = "%s%s" % [quoted_name, "_" * suffix_len]
      self.class.
          where("username like '#{pattern}'").
          select(:username).
          order('username asc').
          each do |other_user|
        if other_user.username > next_username
          break
        elsif other_user.username == next_username
          next_suffix += 1
          next_username = "%s%i" % [basename, next_suffix]
        end
      end
      return next_username if (next_username.size <= pattern.size)
    end
    nil
  end

  def set_initial_username
    email_parts = email.partition("@")
    local_parts = email_parts.first.partition("+")
    if email_parts.any?(&:empty?)
      return
    elsif not local_parts.first.empty?
      base_username = local_parts.first
    else
      base_username = email_parts.first
    end
    base_username.sub!(/^[^A-Za-z]+/, "")
    base_username.gsub!(/[^A-Za-z0-9]/, "")
    unless base_username.empty?
      self.username = find_usable_username_from(base_username)
    end
  end

  def prevent_privilege_escalation
    if current_user.andand.is_admin
      return true
    end
    if self.is_active_changed?
      if self.is_active != self.is_active_was
        logger.warn "User #{current_user.uuid} tried to change is_active from #{self.is_admin_was} to #{self.is_admin} for #{self.uuid}"
        self.is_active = self.is_active_was
      end
    end
    if self.is_admin_changed?
      if self.is_admin != self.is_admin_was
        logger.warn "User #{current_user.uuid} tried to change is_admin from #{self.is_admin_was} to #{self.is_admin} for #{self.uuid}"
        self.is_admin = self.is_admin_was
      end
    end
    true
  end

  def prevent_inactive_admin
    if self.is_admin and not self.is_active
      # There is no known use case for the strange set of permissions
      # that would result from this change. It's safest to assume it's
      # a mistake and disallow it outright.
      raise "Admin users cannot be inactive"
    end
    true
  end

  def search_permissions(start, graph, merged={}, upstream_mask=nil, upstream_path={})
    nextpaths = graph[start]
    return merged if !nextpaths
    return merged if upstream_path.has_key? start
    upstream_path[start] = true
    upstream_mask ||= ALL_PERMISSIONS
    nextpaths.each do |head, mask|
      merged[head] ||= {}
      mask.each do |k,v|
        merged[head][k] ||= v if upstream_mask[k]
      end
      search_permissions(head, graph, merged, upstream_mask.select { |k,v| v && merged[head][k] }, upstream_path)
    end
    upstream_path.delete start
    merged
  end

  def create_oid_login_perm (openid_prefix)
    login_perm_props = { "identity_url_prefix" => openid_prefix}

    # Check oid_login_perm
    oid_login_perms = Link.where(tail_uuid: self.email,
                                   link_class: 'permission',
                                   name: 'can_login').where("head_uuid = ?", self.uuid)

    if !oid_login_perms.any?
      # create openid login permission
      oid_login_perm = Link.create(link_class: 'permission',
                                   name: 'can_login',
                                   tail_uuid: self.email,
                                   head_uuid: self.uuid,
                                   properties: login_perm_props
                                  )
      logger.info { "openid login permission: " + oid_login_perm[:uuid] }
    else
      oid_login_perm = oid_login_perms.first
    end

    return oid_login_perm
  end

  def create_user_repo_link(repo_name)
    # repo_name is optional
    if not repo_name
      logger.warn ("Repository name not given for #{self.uuid}.")
      return
    end

    repo = Repository.where(owner_uuid: uuid, name: repo_name).first_or_create!
    logger.info { "repo uuid: " + repo[:uuid] }
    repo_perm = Link.where(tail_uuid: uuid, head_uuid: repo.uuid,
                           link_class: "permission",
                           name: "can_manage").first_or_create!
    logger.info { "repo permission: " + repo_perm[:uuid] }
    return repo_perm
  end

  # create login permission for the given vm_uuid, if it does not already exist
  def create_vm_login_permission_link(vm_uuid, repo_name)
    # vm uuid is optional
    if vm_uuid
      vm = VirtualMachine.where(uuid: vm_uuid).first

      if not vm
        logger.warn "Could not find virtual machine for #{vm_uuid.inspect}"
        raise "No vm found for #{vm_uuid}"
      end
    else
      return
    end

    logger.info { "vm uuid: " + vm[:uuid] }
    login_attrs = {
      tail_uuid: uuid, head_uuid: vm.uuid,
      link_class: "permission", name: "can_login",
    }

    login_perm = Link.
      where(login_attrs).
      select { |link| link.properties["username"] == repo_name }.
      first

    login_perm ||= Link.
      create(login_attrs.merge(properties: {"username" => repo_name}))

    logger.info { "login permission: " + login_perm[:uuid] }
    login_perm
  end

  # add the user to the 'All users' group
  def create_user_group_link
    return (Link.where(tail_uuid: self.uuid,
                       head_uuid: all_users_group[:uuid],
                       link_class: 'permission',
                       name: 'can_read').first or
            Link.create(tail_uuid: self.uuid,
                        head_uuid: all_users_group[:uuid],
                        link_class: 'permission',
                        name: 'can_read'))
  end

  # Give the special "System group" permission to manage this user and
  # all of this user's stuff.
  def add_system_group_permission_link
    return true if uuid == system_user_uuid
    act_as_system_user do
      Link.create(link_class: 'permission',
                  name: 'can_manage',
                  tail_uuid: system_group_uuid,
                  head_uuid: self.uuid)
    end
  end

  # Send admin notifications
  def send_admin_notifications
    AdminNotifier.new_user(self).deliver
    if not self.is_active then
      AdminNotifier.new_inactive_user(self).deliver
    end
  end

  # Automatically setup new user during creation
  def auto_setup_new_user
    setup_repo_vm_links(nil, nil, Rails.configuration.default_openid_prefix)
    if username
      create_vm_login_permission_link(Rails.configuration.auto_setup_new_users_with_vm_uuid,
                                      username)
      repo_name = "#{username}/#{username}"
      if Rails.configuration.auto_setup_new_users_with_repository and
          Repository.where(name: repo_name).first.nil?
        repo = Repository.create!(name: repo_name, owner_uuid: uuid)
        Link.create!(tail_uuid: uuid, head_uuid: repo.uuid,
                     link_class: "permission", name: "can_manage")
      end
    end
  end

  # Send notification if the user saved profile for the first time
  def send_profile_created_notification
    if self.prefs_changed?
      if self.prefs_was.andand.empty? || !self.prefs_was.andand['profile']
        profile_notification_address = Rails.configuration.user_profile_notification_address
        ProfileNotifier.profile_created(self, profile_notification_address).deliver if profile_notification_address
      end
    end
  end

  def verify_repositories_empty
    unless repositories.first.nil?
      errors.add(:username, "can't be unset when the user owns repositories")
      false
    end
  end

  def sync_repository_names
    old_name_re = /^#{Regexp.escape(username_was)}\//
    name_sub = "#{username}/"
    repositories.find_each do |repo|
      repo.name = repo.name.sub(old_name_re, name_sub)
      repo.save!
    end
  end
end
