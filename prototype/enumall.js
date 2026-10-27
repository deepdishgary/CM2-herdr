const HID = require('node-hid');
for (const d of HID.devices()) {
  console.log(`${(d.vendorId||0).toString(16)}:${(d.productId||0).toString(16)} up=0x${(d.usagePage||0).toString(16)} u=${d.usage} "${d.manufacturer||''}" "${d.product||''}" path=${d.path}`);
}
