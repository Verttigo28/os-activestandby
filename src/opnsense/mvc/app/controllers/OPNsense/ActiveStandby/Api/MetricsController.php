<?php

namespace OPNsense\ActiveStandby\Api;

use OPNsense\Base\ApiControllerBase;
use OPNsense\Core\Backend;

/**
 * Prometheus text exposition of the HA state: scrape https://<node>/api/activestandby/metrics/index
 * with an OPNsense API key (basic auth). One scrape target per node.
 */
class MetricsController extends ApiControllerBase
{
    public function indexAction()
    {
        $text = (new Backend())->configdRun('activestandby ctl_metrics');
        header('Content-Type: text/plain; version=0.0.4; charset=utf-8');
        echo $text;
        exit;
    }
}
