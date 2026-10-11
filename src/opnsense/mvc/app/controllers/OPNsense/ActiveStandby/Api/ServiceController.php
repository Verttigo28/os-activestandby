<?php

namespace OPNsense\ActiveStandby\Api;

use OPNsense\Base\ApiMutableServiceControllerBase;

class ServiceController extends ApiMutableServiceControllerBase
{
    protected static $internalServiceClass = '\OPNsense\ActiveStandby\ActiveStandby';
    protected static $internalServiceTemplate = 'OPNsense/ActiveStandby';
    protected static $internalServiceEnabled = 'general.enabled';
    protected static $internalServiceName = 'activestandby';
}
