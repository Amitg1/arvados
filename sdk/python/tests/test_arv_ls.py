#!/usr/bin/env python
# -*- coding: utf-8 -*-

import io
import os
import random
import sys
import mock
import tempfile
import multiprocessing

import arvados.errors as arv_error
import arvados.commands.ls as arv_ls
import run_test_server

from arvados_testutil import str_keep_locator

class ArvLsTestCase(run_test_server.TestCaseWithServers):
    FAKE_UUID = 'zzzzz-4zz18-12345abcde12345'

    def newline_join(self, seq):
        return '\n'.join(seq) + '\n'

    def random_blocks(self, *sizes):
        return ' '.join('{:032x}+{:d}'.format(
                  random.randint(0, (16 ** 32) - 1), size
                ) for size in sizes)

    def mock_api_for_manifest(self, manifest_lines, uuid=FAKE_UUID):
        manifest_text = self.newline_join(manifest_lines)
        pdh = str_keep_locator(manifest_text)
        coll_info = {'uuid': uuid,
                     'portable_data_hash': pdh,
                     'manifest_text': manifest_text}
        api_client = mock.MagicMock(name='mock_api_client')
        api_client.collections().get().execute.return_value = coll_info
        return coll_info, api_client

    def run_ls(self, args, api_client):
        self.stdout = io.BytesIO()
        self.stderr = io.BytesIO()
        return arv_ls.main(args, self.stdout, self.stderr, api_client)

    def run_ls_process(self, args=[], api_client=None):
        _, stdout_path = tempfile.mkstemp()
        _, stderr_path = tempfile.mkstemp()
        def wrap():
            def wrapper(*args, **kwargs):
                sys.stdout = open(stdout_path, 'w')
                sys.stderr = open(stderr_path, 'w')
                arv_ls.main(*args, **kwargs)
            return wrapper
        p = multiprocessing.Process(target=wrap(),
                                    args=(args, sys.stdout, sys.stderr, api_client))
        p.start()
        p.join()
        out = open(stdout_path, 'r').read()
        err = open(stderr_path, 'r').read()
        os.unlink(stdout_path)
        os.unlink(stderr_path)
        return p.exitcode, out, err

    def test_plain_listing(self):
        collection, api_client = self.mock_api_for_manifest(
            ['. {} 0:3:one.txt 3:4:two.txt'.format(self.random_blocks(5, 2)),
             './dir {} 1:5:sub.txt'.format(self.random_blocks(8))])
        self.assertEqual(0, self.run_ls([collection['uuid']], api_client))
        self.assertEqual(
            self.newline_join(['./one.txt', './two.txt', './dir/sub.txt']),
            self.stdout.getvalue())
        self.assertEqual('', self.stderr.getvalue())

    def test_size_listing(self):
        collection, api_client = self.mock_api_for_manifest(
            ['. {} 0:0:0.txt 0:1000:1.txt 1000:2000:2.txt'.format(
                    self.random_blocks(3000))])
        self.assertEqual(0, self.run_ls(['-s', collection['uuid']], api_client))
        self.stdout.seek(0, 0)
        for expected in range(3):
            actual_size, actual_name = self.stdout.readline().split()
            # But she seems much bigger to me...
            self.assertEqual(str(expected), actual_size)
            self.assertEqual('./{}.txt'.format(expected), actual_name)
        self.assertEqual('', self.stdout.read(-1))
        self.assertEqual('', self.stderr.getvalue())

    def test_nonnormalized_manifest(self):
        collection, api_client = self.mock_api_for_manifest(
            ['. {} 0:1010:non.txt'.format(self.random_blocks(1010)),
             '. {} 0:2020:non.txt'.format(self.random_blocks(2020))])
        self.assertEqual(0, self.run_ls(['-s', collection['uuid']], api_client))
        self.stdout.seek(0, 0)
        self.assertEqual(['3', './non.txt'], self.stdout.readline().split())
        self.assertEqual('', self.stdout.read(-1))
        self.assertEqual('', self.stderr.getvalue())

    def test_locator_failure(self):
        api_client = mock.MagicMock(name='mock_api_client')
        api_client.collections().get().execute.side_effect = (
            arv_error.NotFoundError)
        self.assertNotEqual(0, self.run_ls([self.FAKE_UUID], api_client))
        self.assertNotEqual('', self.stderr.getvalue())

    def test_version_argument(self):
        _, api_client = self.mock_api_for_manifest([''])
        exitcode, out, err = self.run_ls_process(['--version'])
        self.assertEqual(0, exitcode)
        self.assertEqual('', out)
        self.assertNotEqual('', err)
        self.assertRegexpMatches(err, "[0-9]+\.[0-9]+\.[0-9]+")
