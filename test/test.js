const wdio = require("webdriverio");
const fetch = require("node-fetch");


const BASE_IP = "192.168.48.146:8080";
const DEVICE_ID = "00008030-001454190EEB802E";
const DOMAIN = "https://mfarm.dev.rnd.lanit.ru";
const TARGET_STREAM = { url: "192.168.48.139", port: 5004 };
const SELENIUM_HUB = "hub.dev.rnd.lanit.ru";
const DEVICE_CAPS = {
  platformName: "iOS",
  "appium:automationName": "XCUITest",
  "appium:deviceName": "00008030-001454190EEB802E",
  "appium:platformVersion": "18.5",
  "appium:udid": "00008030-001454190EEB802E",
  "xFarm:runId": "local",
  "appium:usePrebuiltWDA": true,
  "appium:app": "https://nexus.rnd.lanit.ru/repository/share/testapp2.ipa",
  "appium:autoGrantPermissions": true,
  "appium:bundleId": "lanit.testapp"
};

async function apiFetch(path, method = "GET", body = null, headers = { "Content-Type": "application/json" }) {
  const options = { method, headers };
  if (body) options.body = JSON.stringify(body);

  const response = await fetch(`http://${BASE_IP}${path}`, options);
  if (!response.ok) throw new Error(`Request failed: ${response.status} ${response.statusText}`);
  return response.json();
}

const getDeviceCapabilitiesFromFarmApi = async (deviceType, identifier) => {
  const url = `${DOMAIN}/api/farm/device/hold/${deviceType}/${identifier}/5`;
  const response = await fetch(url, { method: 'PATCH' });
  if (!response.ok) throw new Error(`Failed to get device capabilities: ${response.statusText}`);
  return response.json();
};

const startStream = () => apiFetch(`/api/v1/device/${DEVICE_ID}/stream/start`, "POST", TARGET_STREAM);
const stopStream = () => apiFetch(`/api/v1/device/${DEVICE_ID}/stream/stop`, "DELETE");
const startWda = () => apiFetch(`/api/v1/device/${DEVICE_ID}/wda/session`, "POST");
const stopWda = () => apiFetch(`/api/v1/device/${DEVICE_ID}/wda/session`, "DELETE");
const installApp = () => apiFetch(`/api/v1/device/${DEVICE_ID}/apps/install`, "POST");
const deleteApp = () => apiFetch(`/api/v1/device/${DEVICE_ID}/apps/uninstall`, "DELETE");
const launchApp = () => apiFetch(`/api/v1/device/${DEVICE_ID}/apps/install`, "POST");
const stopApp = () => apiFetch(`/api/v1/device/${DEVICE_ID}/apps/install`, "POST");


async function testHelloWorld(client) {
  const helloText = await client.$(
    `-ios predicate string:type == "XCUIElementTypeStaticText" AND label == "Hello, world!"`
  );

  const isDisplayed = await helloText.isDisplayed();
  await client.saveScreenshot("hello.png");

  console.log(
    isDisplayed
      ? '✅ Текст "Hello, world!" найден!'
      : '❌ Текст "Hello, world!" не найден!'
  );
}

async function testCounterIncrement(client) {
  // Находим лейбл счётчика
  const counterLabel = await client.$(
    `-ios predicate string:type == "XCUIElementTypeStaticText" AND label BEGINSWITH "Счётчик:"`
  );

  let labelText = await counterLabel.getText();
  console.log("Начальное значение:", labelText);

  // Находим кнопку (лучше через accessibility id, например ~increment)
  const incrementBtn = await client.$(`-ios predicate string:type == "XCUIElementTypeButton"`);
  await incrementBtn.click();

  // Проверяем текст снова
  const newLabelText = await counterLabel.getText();
  await client.saveScreenshot("counter.png");

  if (newLabelText !== labelText) {
    console.log(`✅ Счётчик изменился: ${labelText} → ${newLabelText}`);
  } else {
    console.error("❌ Счётчик не изменился!");
  }
}

async function main() {
  try {
    // await stopWda();
    // await startWda();
    const client = await wdio.remote({
      protocol: "https",
      hostname: SELENIUM_HUB,
      port: 443,
      path: "/wd/hub",
      capabilities: DEVICE_CAPS,
      connectionRetryCount: 3
    });

    await testHelloWorld(client);
    await testCounterIncrement(client);

    await client.deleteSession();
    // await stopWda();
  } catch (err) {
    console.error("Ошибка при запуске теста:", err);
  }
}



main();
