const HID = require('node-hid');
const devs = HID.devices().filter(d => d.vendorId === 0x303a && d.productId === 0x8298);
console.log(JSON.stringify(devs, null, 1));
