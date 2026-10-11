<?php

namespace OPNsense\ActiveStandby\Api;

use OPNsense\Base\ApiControllerBase;
use OPNsense\Core\Config;
use OPNsense\ActiveStandby\ActiveStandby;

class SettingsController extends ApiControllerBase
{
    public function getAction()
    {
        $result = [];
        if ($this->request->isGet()) {
            $mdl = new ActiveStandby();
            $result['activestandby'] = $mdl->getNodes();
        }
        return $result;
    }

    public function setAction()
    {
        $result = ['result' => 'failed'];
        if ($this->request->isPost() && $this->request->hasPost('activestandby')) {
            $mdl = new ActiveStandby();
            $mdl->setNodes($this->request->getPost('activestandby'));
            $valMsgs = $mdl->performValidation();
            if ($valMsgs->count() > 0) {
                $result['validations'] = [];
                foreach ($valMsgs as $msg) {
                    $result['validations']['activestandby.' . $msg->getField()] = $msg->getMessage();
                }
                return $result;
            }
            $h = (string)$mdl->general->ha_interface;
            if ($h !== '' && $h === (string)$mdl->general->mgmt_interface) {
                $result['validations'] = [
                    'activestandby.general.mgmt_interface' => gettext('Management and HA interface must differ.')
                ];
                return $result;
            }
            $mdl->serializeToConfig();
            Config::getInstance()->save();
            $result['result'] = 'saved';
        }
        return $result;
    }
}
