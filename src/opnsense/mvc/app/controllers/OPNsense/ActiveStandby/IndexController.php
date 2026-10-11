<?php

namespace OPNsense\ActiveStandby;

class IndexController extends \OPNsense\Base\IndexController
{
    public function indexAction()
    {
        $this->view->generalForm = $this->getForm('general');
        $this->view->pick('OPNsense/ActiveStandby/index');
    }
}
