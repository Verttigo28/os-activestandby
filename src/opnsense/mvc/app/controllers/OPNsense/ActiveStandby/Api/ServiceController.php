<?php

namespace OPNsense\ActiveStandby\Api;

use OPNsense\Core\Backend;
use OPNsense\Base\ApiMutableServiceControllerBase;

class ServiceController extends ApiMutableServiceControllerBase
{
    protected static $internalServiceClass = '\OPNsense\ActiveStandby\ActiveStandby';
    protected static $internalServiceTemplate = 'OPNsense/ActiveStandby';
    protected static $internalServiceEnabled = 'general.enabled';
    protected static $internalServiceName = 'activestandby';

    public function reconfigureAction()
    {
        $who = preg_replace('/[^A-Za-z0-9._-]/', '', (string)($this->session->get('Username') ?? 'api'));
        $ip = preg_replace('/[^0-9a-fA-F.:]/', '', (string)$this->request->getClientAddress());
        $mark = 'service_reconfigure:by:' . $who . ':from:' . $ip;
        syslog(LOG_NOTICE, 'activestandby audit ' . $mark);
        (new Backend())->configdpRun('activestandby ctl_audit', [$mark]);
        return parent::reconfigureAction();
    }
}
