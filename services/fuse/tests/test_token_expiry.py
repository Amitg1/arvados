import arvados
import apiclient
import logging
import mock
import os
import re
import time
import unittest

from .integration_test import IntegrationTest

logger = logging.getLogger('arvados.arv-mount')

class TokenExpiryTest(IntegrationTest):
    @mock.patch('arvados.keep.KeepClient.get')
    @IntegrationTest.mount(argv=['--mount-by-id', 'zzz'])
    def test_refresh_old_manifest(self, mocked_get):
        mocked_get.return_value = 'fake data'

        self.api._rootDesc["blobSignatureTtl"] = 2
        old_exp = int(time.time()) + 86400*14
        self.pool_test(os.path.join(self.mnt, 'zzz'))
        want_exp = int(time.time()) + 86400*14

        got_loc = mocked_get.call_args[0][0]
        got_exp = int(
            re.search(r'\+A[0-9a-f]+@([0-9a-f]+)', got_loc).group(1),
            16)
        self.assertGreaterEqual(
            got_exp, want_exp-2,
            msg='now+2w = {:x}, but fuse fetched locator {} (old_exp {:x})'.format(
                want_exp, got_loc, old_exp))
        self.assertLessEqual(
            got_exp, want_exp,
            msg='server is not using the expected 2w TTL; test is ineffective')

    @staticmethod
    def _test_refresh_old_manifest(self, zzz):
        uuid = 'zzzzz-4zz18-op4e2lbej01tcvu'
        fnm = 'zzzzz-8i9sb-0vsrcqi7whchuil.log.txt'
        os.listdir(os.path.join(zzz, uuid))
        time.sleep(3)
        with open(os.path.join(zzz, uuid, fnm)) as f:
            f.read()
