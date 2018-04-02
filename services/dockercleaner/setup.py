#!/usr/bin/env python3
# Copyright (C) The Arvados Authors. All rights reserved.
#
# SPDX-License-Identifier: AGPL-3.0

import os
import sys
import setuptools.command.egg_info as egg_info_cmd

from setuptools import setup, find_packages

tagger = egg_info_cmd.egg_info
version = os.environ.get("ARVADOS_BUILDING_VERSION")
if not version:
    version = "0.1"
    try:
        import gittaggers
        tagger = gittaggers.EggInfoFromGit
    except ImportError:
        pass

setup(name="arvados-docker-cleaner",
      version=version,
      description="Arvados Docker cleaner",
      author="Arvados",
      author_email="info@arvados.org",
      url="https://arvados.org",
      download_url="https://github.com/curoverse/arvados.git",
      license="GNU Affero General Public License version 3.0",
      packages=find_packages(),
      entry_points={
          'console_scripts': ['arvados-docker-cleaner=arvados_docker.cleaner:main'],
      },
      data_files=[
          ('share/doc/arvados-docker-cleaner', ['agpl-3.0.txt', 'arvados-docker-cleaner.service']),
      ],
      install_requires=[
          'docker-py==1.7.2',
          'setuptools',
      ],
      tests_require=[
          'pbr<1.7.0',
          'mock',
      ],
      test_suite='tests',
      zip_safe=False,
      cmdclass={'egg_info': tagger},
)
