import assert from "node:assert/strict";
import { chromium } from "../../../sdk/ui/node_modules/@playwright/test/index.mjs";
const fixture=JSON.parse(process.env.CONTENTKIT_BROWSER_FIXTURE);
const scenario=process.argv[2];
const browser=await chromium.launch({headless:true, ...(process.env.CONTENTKIT_CRITICAL_CHROMIUM?{executablePath:process.env.CONTENTKIT_CRITICAL_CHROMIUM}:{})});
try {
  const page=await browser.newPage();
  await page.goto(fixture.url);
  await page.waitForFunction(()=>!!window.critical);
  await page.evaluate(input=>window.critical.initialize(input),fixture);
  async function hold(pattern,method) {
    let reached, release, delivered;
    const seen=new Promise(resolve=>reached=resolve), gate=new Promise(resolve=>release=resolve), done=new Promise(resolve=>delivered=resolve);
    await page.route(pattern,async route=>{
      if(route.request().method()!==method){await route.continue();return;}
      const actual=await route.fetch();
      assert.equal(actual.status(),200,"delayed upstream must have succeeded");
      const body=await actual.json();
      if(scenario==="CK-J29-S03")assert.equal(body.favorited,true,"held response must contain Alice's private state");
      if(scenario==="CK-J29-S04")assert.equal(body.favorited,false,"held write must already be accepted");
      if(scenario==="CK-J29-S05")assert.equal(body.id,fixture.item,"canceled read must have reached the real handler");
      reached();await gate;
      await route.fulfill({response:actual}).catch(error=>{if(!/closed|aborted|intercept/i.test(String(error)))throw error;});
      delivered();
    },{times:1});
    return {seen,release,done};
  }
  if(scenario==="CK-J29-S02") {
    assert.match(await page.evaluate(()=>window.critical.binding()),/^[a-f0-9-]{36}$/);
  } else if(scenario==="CK-J29-S03") {
    const held=await hold(`**/contents/${fixture.item}/favorite`,"GET");
    await page.evaluate(()=>window.critical.mount());await held.seen;
    await page.evaluate(()=>window.critical.switchToBob());
    held.release();await held.done;
    await page.waitForFunction(bob=>{const view=document.querySelector("#favorite");return view?.dataset.viewer===bob&&view.dataset.loading==="false"&&view.textContent==="false";},fixture.bob);
    // A fresh new-scope HTTP read confirms the rendered value is Bob's.
    assert.equal(await page.evaluate(id=>window.client.favorites.get({type:"contents",id}).then(x=>x.favorited),fixture.item),false);
    assert.deepEqual(await page.evaluate(()=>window.leaks),[],"Alice's late state appeared transiently in Bob's view");
  } else if(scenario==="CK-J29-S04") {
    const held=await hold(`**/contents/${fixture.item}/favorite`,"DELETE");
    await page.evaluate(()=>window.critical.beginMutation());await held.seen;
    await page.evaluate(()=>window.critical.switchScope());held.release();
    const changes=await page.evaluate(()=>window.critical.finishMutation());
    assert.equal(changes.length,1);assert.equal(changes[0].scope,null);assert.equal(changes[0].change.type,"favorite.changed");
  } else if(scenario==="CK-J29-S05") {
    const held=await hold(`**/contents/${fixture.item}`,"GET");
    await page.evaluate(()=>window.critical.beginCancellation());await held.seen;
    await page.evaluate(()=>window.critical.cancel());held.release();
    assert.deepEqual(await page.evaluate(()=>window.critical.cancellation()),{code:"aborted",status:0});
  } else if(scenario==="CK-J29-S06") {
    const error=await page.evaluate(()=>window.critical.stableErrors());assert.equal(error.status,413);assert.equal(error.code,"too_large");
  } else if(scenario==="CK-J09-S01") {
    assert.match(await page.evaluate(()=>window.critical.imageUpload()),/^[a-f0-9-]{36}$/);
  } else throw Error(`unknown browser scenario ${scenario}`);
  console.log(JSON.stringify({scenario,status:"passed"}));
} finally {await browser.close();}
