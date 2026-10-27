import assert from "node:assert/strict";
import { chromium } from "../../../sdk/ui/node_modules/@playwright/test/index.mjs";
import { resolveInlineImageReferences } from "../../../sdk/ui/dist/client.js";
const fixture=JSON.parse(process.env.CONTENTKIT_BROWSER_FIXTURE);
const scenario=process.argv[2];
const browser=await chromium.launch({headless:true, ...(process.env.CONTENTKIT_CRITICAL_CHROMIUM?{executablePath:process.env.CONTENTKIT_CRITICAL_CHROMIUM}:{})});
try {
  const page=await browser.newPage();
  const debug=[];
  const account=request=>{const bearer=request.headers().authorization;return bearer===`Bearer ${fixture.aliceToken}`?"alice":bearer===`Bearer ${fixture.bobToken}`?"bob":"other";};
  page.on("request",request=>{if(request.url().includes("/favorite"))debug.push({event:"request",method:request.method(),account:account(request)});});
  page.on("requestfailed",request=>{if(request.url().includes("/favorite"))debug.push({event:"failed",account:account(request),error:request.failure()?.errorText});});
  page.on("response",response=>{if(response.url().includes("/favorite"))debug.push({event:"response",status:response.status(),account:account(response.request())});});
  page.on("pageerror",error=>debug.push({event:"pageerror",error:String(error)}));
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
    try {
    await page.waitForFunction(bob=>{const view=document.querySelector("#favorite");return view?.dataset.viewer===bob&&view.dataset.loading==="false"&&view.textContent==="false";},fixture.bob);
    } catch (error) {
      const rendered=await page.locator("#favorite").evaluate(view=>({text:view.textContent,viewer:view.dataset.viewer,loading:view.dataset.loading}));
      throw new Error(`Account-switch favorite did not settle: ${JSON.stringify({rendered,debug,samples:await page.evaluate(()=>window.favoriteSamples),leaks:await page.evaluate(()=>window.leaks)})}`,{cause:error});
    }
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
  } else if(scenario==="CK-J29-S09") {
    const serverHTML='<img src="contentkit:i-11111111-1111-4111-8111-111111111111">';
    assert.equal(typeof DOMParser,"undefined");
    assert.equal(resolveInlineImageReferences(serverHTML,{}),serverHTML,"server rendering must not require DOMParser");
    const inline=await page.evaluate(()=>window.critical.inlineImages());
    assert.deepEqual(inline.en.sources,[inline.enURL,null]);
    assert.deepEqual(inline.zh.sources,[inline.zhURL,null]);
    for(const unsafe of inline.unsafe)assert.deepEqual(unsafe.sources,[null,null]);
    assert.equal(inline.plain,inline.plainHTML,"ordinary markup must remain unchanged");
    assert.deepEqual(inline.en.handlers,[null,null]);
    assert.equal(inline.en.text,`keep literal contentkit:${inline.id}`);
    assert.deepEqual(inline.quill.ops,[{insert:{image:inline.zhURL}},{insert:`literal contentkit:${inline.id}`}]);
    assert.deepEqual(JSON.parse(resolveInlineImageReferences(JSON.stringify({ops:[{insert:{image:`contentkit:${inline.id}`}}]}),{[inline.id]:inline.enURL},"quill")),{ops:[{insert:{image:inline.enURL}}]},"Quill resolution must also work during server rendering");
    await page.evaluate(()=>window.critical.mountVersionEditor());
    const title=page.getByRole("textbox",{name:"Title",exact:true});
    const body=page.getByRole("textbox",{name:"Body",exact:true});
    await page.waitForFunction(()=>document.querySelector('[aria-label="Body"]')?.value==="English body only");
    assert.equal(await title.inputValue(),"English edition");
    await page.getByRole("combobox",{name:"Versions",exact:true}).click();
    await page.getByRole("option",{name:"Chinese",exact:true}).click();
    await page.waitForFunction(()=>document.querySelector('[aria-label="Body"]')?.value==="中文内容");
    assert.equal(await title.inputValue(),"中文版本");
    await title.fill("中文编辑");await body.fill("只有中文更新");
    const saved=page.waitForResponse(response=>response.request().method()==="PATCH" && response.url().includes(`/versions/${fixture.zhVersion}`));
    await page.getByRole("button",{name:"Save",exact:true}).first().click();
    assert.equal((await saved).status(),200);
    await page.getByRole("combobox",{name:"Versions",exact:true}).click();
    await page.getByRole("option",{name:"English",exact:true}).click();
    await page.waitForFunction(()=>document.querySelector('[aria-label="Body"]')?.value==="English body only");
    assert.equal(await title.inputValue(),"English edition");
    const editions=await page.evaluate(async fixture=>({en:await window.client.contents.getByVersion(fixture.enVersion,"en"),zh:await window.client.contents.getByVersion(fixture.zhVersion,"zh")}),fixture);
    assert.equal(editions.en.body,"English body only");assert.equal(editions.zh.body,"只有中文更新");
    await page.evaluate(version=>window.critical.mountVersionReader(version),fixture.zhVersion);
    await page.waitForFunction(version=>{const el=document.querySelector("#version-reader");return el?.dataset.version===version&&el.dataset.loading==="false"&&el.textContent==="只有中文更新";},fixture.zhVersion);
    await page.evaluate(version=>window.critical.mountVersionReader(version),fixture.enVersion);
    await page.waitForFunction(version=>{const el=document.querySelector("#version-reader");return el?.dataset.version===version&&el.dataset.loading==="false"&&el.textContent==="English body only";},fixture.enVersion);
  } else throw Error(`unknown browser scenario ${scenario}`);
  console.log(JSON.stringify({scenario,status:"passed"}));
} finally {await browser.close();}
