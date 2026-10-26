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
  } else if(scenario==="CK-J29-S08") {
    assert.equal(await page.evaluate(()=>window.critical.uploadChannelImage()),fixture.destination);
    await page.evaluate(()=>window.critical.mountChannel());
    await page.locator("#empty-channel").waitFor();
    await page.getByRole("button", {name:"Repost",exact:true}).click();
    const dialog=page.getByRole("dialog");
    await dialog.getByRole("button", {name:"Repost",exact:true}).click();
    await dialog.waitFor({state:"hidden"});
    await page.locator('[data-ckui="card-title"]').filter({hasText:"Browser canonical original"}).waitFor();
    assert.match(await page.locator('[data-ckui="repost-context"]').innerText(),/Reposted by Destination channel/);
    const feed=await page.evaluate(id=>window.client.channels.contents(id),fixture.destination);
    assert.equal(feed.contents.length,1);
    assert.equal(feed.contents[0].id,fixture.item);
    assert.equal(feed.contents[0].channel.id,fixture.originalChannel);
    await page.getByRole("button", {name:"Remove repost",exact:true}).click();
    await dialog.getByRole("button", {name:"Remove repost",exact:true}).click();
    await dialog.waitFor({state:"hidden"});
    await page.locator("#empty-channel").waitFor();
    const original=await page.evaluate(id=>window.client.contents.get(id),fixture.item);
    assert.equal(original.id,fixture.item);
    assert.equal(original.channel_id,fixture.originalChannel);
    assert.equal(original.editor,false,"reposting must not grant editing authority");
  } else throw Error(`unknown browser scenario ${scenario}`);
  console.log(JSON.stringify({scenario,status:"passed"}));
} finally {await browser.close();}
