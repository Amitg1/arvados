require 'integration_helper'

class WebsocketTest < ActionDispatch::IntegrationTest
  setup do
    need_selenium "to make websockets work"
  end

  test "test page" do
    visit(page_with_token("active", "/websockets"))
    fill_in("websocket-message-content", :with => "Stuff")
    click_button("Send")
    assert_text '"status":400'
  end

  test "test live logging" do
    visit(page_with_token("active", "/pipeline_instances/zzzzz-d1hrv-9fm8l10i9z2kqc6"))
    click_link("Log")
    assert_no_text '123 hello'

    api = ArvadosApiClient.new

    use_token :active
    api.api("logs", "", {log: {
                object_uuid: "zzzzz-d1hrv-9fm8l10i9z2kqc6",
                event_type: "stderr",
                properties: {"text" => "123 hello"}}})
    assert_text '123 hello'
  end

  [
   ["pipeline_instances", api_fixture("pipeline_instances")['pipeline_with_newer_template']['uuid']],
   ["jobs", api_fixture("jobs")['running']['uuid']],
   ["containers", api_fixture("containers")['running']['uuid']],
   ["container_requests", api_fixture("container_requests")['running']['uuid'], api_fixture("containers")['running']['uuid']],
  ].each do |controller, uuid, log_uuid|
    log_uuid = log_uuid || uuid

    test "test live logging scrolling for #{controller}" do

      visit(page_with_token("active", "/#{controller}/#{uuid}"))
      click_link("Log")
      assert_no_text '123 hello'

      api = ArvadosApiClient.new

      text = ""
      (1..1000).each do |i|
        text << "#{i} hello\n"
      end

      use_token :dispatch1 do
        api.api("logs", "", {log: {
                    object_uuid: log_uuid,
                    event_type: "stderr",
                    properties: {"text" => text}}})
      end
      assert_text '1000 hello'

      # First test that when we're already at the bottom of the page, it scrolls down
      # when a new line is added.
      old_top = page.evaluate_script("$('#event_log_div').scrollTop()")

      api.api("logs", "", {log: {
                  object_uuid: log_uuid,
                  event_type: "stderr",
                  properties: {"text" => "1001 hello\n"}}})
      assert_text '1001 hello'

      # Check that new value of scrollTop is greater than the old one
      new_top = page.evaluate_script("$('#event_log_div').scrollTop()")
      assert_operator new_top, :>, old_top

      # Now scroll to 30 pixels from the top
      page.execute_script "$('#event_log_div').scrollTop(30)"
      assert_equal 30, page.evaluate_script("$('#event_log_div').scrollTop()")

      api.api("logs", "", {log: {
                  object_uuid: log_uuid,
                  event_type: "stderr",
                  properties: {"text" => "1002 hello\n"}}})
      assert_text '1002 hello'

      # Check that we haven't changed scroll position
      assert_equal 30, page.evaluate_script("$('#event_log_div').scrollTop()")
    end
  end

  test "pipeline instance arv-refresh-on-log-event" do
    use_token :active
    # Do something and check that the pane reloads.
    p = PipelineInstance.create({state: "RunningOnServer",
                                  components: {
                                    c1: {
                                      script: "test_hash.py",
                                      script_version: "1de84a854e2b440dc53bf42f8548afa4c17da332"
                                    }
                                  }
                                })

    visit(page_with_token("active", "/pipeline_instances/#{p.uuid}"))

    assert_text 'Active'
    assert page.has_link? 'Pause'
    assert_no_text 'Complete'
    assert page.has_no_link? 'Re-run with latest'

    use_token :admin do
      p.state = "Complete"
      p.save!
    end

    assert_no_text 'Active'
    assert page.has_no_link? 'Pause'
    assert_text 'Complete'
    assert page.has_link? 'Re-run with latest'
  end

  test "job arv-refresh-on-log-event" do
    use_token :active
    # Do something and check that the pane reloads.
    p = Job.where(uuid: api_fixture('jobs')['running_will_be_completed']['uuid']).results.first

    visit(page_with_token("active", "/jobs/#{p.uuid}"))

    assert_no_text 'complete'
    assert_no_text 'Re-run job'

    use_token :admin do
      p.state = "Complete"
      p.save!
    end

    assert_text 'complete'
    assert_text 'Re-run job'
  end

  test "dashboard arv-refresh-on-log-event" do
    use_token :active

    visit(page_with_token("active", "/"))

    assert_no_text 'test dashboard arv-refresh-on-log-event'

    # Do something and check that the pane reloads.
    p = PipelineInstance.create({state: "RunningOnServer",
                                  name: "test dashboard arv-refresh-on-log-event",
                                  components: {
                                  }
                                })

    assert_text 'test dashboard arv-refresh-on-log-event'
  end

  test 'job graph appears when first data point is already in logs table' do
    job_graph_first_datapoint_test
  end

  test 'job graph appears when first data point arrives by websocket' do
    use_token :admin do
      Log.find(api_fixture('logs')['crunchstat_for_running_job']['uuid']).destroy
    end
    job_graph_first_datapoint_test expect_existing_datapoints: false
  end

  def job_graph_first_datapoint_test expect_existing_datapoints: true
    uuid = api_fixture('jobs')['running']['uuid']

    visit page_with_token "active", "/jobs/#{uuid}"
    click_link "Log"

    assert_selector '#event_log_div', visible: true

    if expect_existing_datapoints
      assert_selector '#log_graph_div', visible: true
      # Magic numbers 12.99 etc come from the job log fixture:
      assert_last_datapoint 'T1-cpu', (((12.99+0.99)/10.0002)/8)
    else
      # Until graphable data arrives, we should see the text log but not the graph.
      assert_no_selector '#log_graph_div', visible: true
    end

    text = "2014-11-07_23:33:51 #{uuid} 31708 1 stderr crunchstat: cpu 1970.8200 user 60.2700 sys 8 cpus -- interval 10.0002 seconds 35.3900 user 0.8600 sys"

    assert_triggers_dom_event 'arv-log-event' do
      use_token :active do
        api = ArvadosApiClient.new
        api.api("logs", "", {log: {
                    object_uuid: uuid,
                    event_type: "stderr",
                    properties: {"text" => text}}})
      end
    end

    # Graph should have appeared (even if it hadn't above). It's
    # important not to wait like matchers usually do: we are
    # confirming the graph is visible _immediately_ after the first
    # data point arrives.
    using_wait_time 0 do
      assert_selector '#log_graph_div', visible: true
    end
    assert_last_datapoint 'T1-cpu', (((35.39+0.86)/10.0002)/8)
  end

  test "live log charting from replayed log" do
    uuid = api_fixture("jobs")['running']['uuid']

    visit page_with_token "active", "/jobs/#{uuid}"
    click_link "Log"

    assert_triggers_dom_event 'arv-log-event' do
      ApiServerForTests.new.run_rake_task("replay_job_log", "test/job_logs/crunchstatshort.log,1.0,#{uuid}")
    end

    assert_last_datapoint 'T1-cpu', (((35.39+0.86)/10.0002)/8)
  end

  def assert_last_datapoint series, value
    datum = page.evaluate_script("jobGraphData[jobGraphData.length-1]['#{series}']")
    assert_in_epsilon value, datum.to_f
  end

  test "test running job with just a few previous log records" do
    use_token :active
    job = Job.where(uuid: api_fixture("jobs")['running']['uuid']).results.first
    visit page_with_token("active", "/jobs/#{job.uuid}")

    api = ArvadosApiClient.new

    # Create just one old log record
    api.api("logs", "", {log: {
                object_uuid: job.uuid,
                event_type: "stderr",
                properties: {"text" => "Historic log message"}}})

    click_link("Log")

    # Expect "all" historic log records because we have less than
    # default Rails.configuration.running_job_log_records_to_fetch count
    assert_text 'Historic log message'

    # Create new log record and expect it to show up in log tab
    api.api("logs", "", {log: {
                object_uuid: job.uuid,
                event_type: "stderr",
                properties: {"text" => "Log message after subscription"}}})
    assert_text 'Log message after subscription'
  end

  test "test running job with too many previous log records" do
    Rails.configuration.running_job_log_records_to_fetch = 5

    use_token :active
    job = Job.where(uuid: api_fixture("jobs")['running']['uuid']).results.first

    visit page_with_token("active", "/jobs/#{job.uuid}")

    api = ArvadosApiClient.new

    # Create Rails.configuration.running_job_log_records_to_fetch + 1 log records
    (0..Rails.configuration.running_job_log_records_to_fetch).each do |count|
      api.api("logs", "", {log: {
                object_uuid: job.uuid,
                event_type: "stderr",
                properties: {"text" => "Old log message #{count}"}}})
    end

    # Go to log tab, which results in subscribing to websockets
    click_link("Log")

    # Expect all but the first historic log records,
    # because that was one too many than fetch count.
    (1..Rails.configuration.running_job_log_records_to_fetch).each do |count|
      assert_text "Old log message #{count}"
    end
    assert_no_text 'Old log message 0'

    # Create one more log record after subscription
    api.api("logs", "", {log: {
                object_uuid: job.uuid,
                event_type: "stderr",
                properties: {"text" => "Life goes on!"}}})
    # Expect it to show up in log tab
    assert_text 'Life goes on!'
  end
end
