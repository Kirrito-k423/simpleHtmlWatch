const {defineConfig} = require('@playwright/test');
module.exports = defineConfig({
  testDir:'./tests/browser',
  workers:1,
  timeout:30000,
  use:{baseURL:'http://127.0.0.1:18767', viewport:{width:2530,height:1100}, acceptDownloads:true},
  projects:[{name:'chromium', use:{browserName:'chromium'}}, {name:'firefox', use:{browserName:'firefox'}}, {name:'webkit', use:{browserName:'webkit'}}],
  webServer:{
    command:'go test ./internal/watch -run ^TestTaskDashboardBrowserFixture$ -count=1 -v',
    env:{SHW_BROWSER_FIXTURE:'1'},
    url:'http://127.0.0.1:18767',
    timeout:60000,
    reuseExistingServer:false,
  },
});
