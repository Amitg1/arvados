#!/usr/bin/env python

import multiprocessing
import os
import sys
import tempfile
import unittest

import arvados.errors as arv_error
import arvados.commands.ws as arv_ws

class ArvWsTestCase(unittest.TestCase):
    def run_ws(self, args):
        return arv_ws.main(args)

    def run_ws_process(self, args=[], api_client=None):
        _, stdout_path = tempfile.mkstemp()
        _, stderr_path = tempfile.mkstemp()
        def wrap():
            def wrapper(*args, **kwargs):
                sys.stdout = open(stdout_path, 'w')
                sys.stderr = open(stderr_path, 'w')
                arv_ws.main(*args, **kwargs)
            return wrapper
        p = multiprocessing.Process(target=wrap(), args=(args,))
        p.start()
        p.join()
        out = open(stdout_path, 'r').read()
        err = open(stderr_path, 'r').read()
        os.unlink(stdout_path)
        os.unlink(stderr_path)
        return p.exitcode, out, err

    def test_unsupported_arg(self):
        with self.assertRaises(SystemExit):
            self.run_ws(['-x=unknown'])

    def test_version_argument(self):
        exitcode, out, err = self.run_ws_process(['--version'])
        self.assertEqual(0, exitcode)
        self.assertEqual('', out)
        self.assertNotEqual('', err)
        self.assertRegexpMatches(err, "[0-9]+\.[0-9]+\.[0-9]+")
