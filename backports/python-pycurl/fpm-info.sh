# Copyright (C) The Arvados Authors. All rights reserved.
#
# SPDX-License-Identifier: Apache-2.0

case "$TARGET" in
    debian8)
            fpm_depends+=(
                libc6
                libcomerr2
                libcurl3-gnutls
                libffi6
                libgcrypt20
                libgmp10
                libgnutls-deb0-28
                libgpg-error0
                libgssapi-krb5-2
                libhogweed2
                libidn11
                libk5crypto3
                libkeyutils1
                libkrb5-3
                libkrb5support0
                libldap-2.4-2
                libnettle4
                libp11-kit0
                librtmp1
                libsasl2-2
                libssh2-1
                libtasn1-6
                zlib1g
            ) ;;
    ubuntu1204)
            fpm_depends+=(
                libasn1-8-heimdal
                libc6
                libcomerr2
                libcurl3-gnutls
                libgcrypt11
                libgnutls26
                libgpg-error0
                libgssapi-krb5-2
                libgssapi3-heimdal
                libhcrypto4-heimdal
                libheimbase1-heimdal
                libheimntlm0-heimdal
                libhx509-5-heimdal
                libidn11
                libk5crypto3
                libkeyutils1
                libkrb5-26-heimdal
                libkrb5-3
                libkrb5support0
                libldap-2.4-2
                libp11-kit0
                libroken18-heimdal
                librtmp0
                libsasl2-2
                libsqlite3-0
                libtasn1-3
                libwind0-heimdal
                zlib1g
            ) ;;
    ubuntu1404)
            fpm_depends+=(
                libasn1-8-heimdal
                libc6
                libcomerr2
                libcurl3-gnutls
                libffi6
                libgcrypt11
                libgnutls26
                libgpg-error0
                libgssapi-krb5-2
                libgssapi3-heimdal
                libhcrypto4-heimdal
                libheimbase1-heimdal
                libheimntlm0-heimdal
                libhx509-5-heimdal
                libidn11
                libk5crypto3
                libkeyutils1
                libkrb5-26-heimdal
                libkrb5-3
                libkrb5support0
                libldap-2.4-2
                libp11-kit0
                libroken18-heimdal
                librtmp0
                libsasl2-2
                libsqlite3-0
                libtasn1-6
                libwind0-heimdal
                zlib1g
            ) ;;
esac
