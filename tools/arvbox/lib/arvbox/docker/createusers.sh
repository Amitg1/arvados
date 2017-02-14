#!/bin/bash

set -e -o pipefail

if ! grep "^arvbox:" /etc/passwd >/dev/null 2>/dev/null ; then
    HOSTUID=$(ls -nd /usr/src/arvados | sed 's/ */ /' | cut -d' ' -f4)
    HOSTGID=$(ls -nd /usr/src/arvados | sed 's/ */ /' | cut -d' ' -f5)

    mkdir -p /var/lib/arvados/git /var/lib/gems \
          /var/lib/passenger /var/lib/gopath /var/lib/pip

    groupadd --gid $HOSTGID --non-unique arvbox
    groupadd --gid $HOSTGID --non-unique git
    useradd --home-dir /var/lib/arvados \
            --uid $HOSTUID --gid $HOSTGID \
            --non-unique \
            --groups docker \
            arvbox
    useradd --home-dir /var/lib/arvados/git --uid $HOSTUID --gid $HOSTGID --non-unique git
    useradd --groups docker crunch

    chown arvbox:arvbox -R /usr/local /var/lib/arvados /var/lib/gems \
          /var/lib/passenger /var/lib/postgresql \
          /var/lib/nginx /var/log/nginx /etc/ssl/private \
          /var/lib/gopath /var/lib/pip

    mkdir -p /var/lib/gems/ruby
    chown arvbox:arvbox -R /var/lib/gems/ruby

    mkdir -p /tmp/crunch0 /tmp/crunch1
    chown crunch:crunch -R /tmp/crunch0 /tmp/crunch1

    echo "arvbox    ALL=(crunch) NOPASSWD: ALL" >> /etc/sudoers
fi

if ! grep "^fuse:" /etc/group >/dev/null 2>/dev/null ; then
    if test -c /dev/fuse ; then
        FUSEGID=$(ls -nd /dev/fuse | sed 's/ */ /' | cut -d' ' -f5)
        groupadd --gid $FUSEGID --non-unique fuse
        useradd --groups fuse arvbox
        useradd --groups fuse crunch
    fi
fi
