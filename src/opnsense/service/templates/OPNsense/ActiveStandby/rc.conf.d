{% if helpers.exists('OPNsense.ActiveStandby.general.enabled') and OPNsense.ActiveStandby.general.enabled == "1" %}
hasyncd_enable="YES"
{% else %}
hasyncd_enable="NO"
{% endif %}
